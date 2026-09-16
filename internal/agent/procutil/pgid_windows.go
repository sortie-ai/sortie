//go:build windows

package procutil

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobEntry holds the Windows Job Object handle and the process
// reference associated with a managed process. The proc field is
// always set by registerJobAssignment; the job field is zero when Job
// Object creation failed (degraded mode).
type jobEntry struct {
	job  windows.Handle
	proc *os.Process
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

// dwordPID narrows a Go process identifier to the DWORD that the
// Win32 process APIs take. [os.Process] reports a pid as an int,
// while Windows identifies a process by a 32-bit unsigned value, so
// a pid outside that range names no live process. Converting one
// unchecked would wrap it onto an unrelated identifier and signal or
// terminate the wrong process tree.
func dwordPID(pid int) (uint32, error) {
	if pid < 0 || pid > math.MaxUint32 {
		return 0, fmt.Errorf("process id %d is outside the windows process identifier range", pid)
	}
	return uint32(pid), nil
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
		group, err := dwordPID(pid)
		if err != nil {
			return err
		}
		return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, group)
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
// associated with pid. If assignment succeeded but the Job Object
// handle is for some reason zero (should not happen), falls back to
// killing the single process via its stored *os.Process reference.
// If no entry is registered (the process was never assigned), returns
// nil: the process either already exited or was never started.
//
// LoadAndDelete atomicity ensures exactly one concurrent caller gets
// the handle when RunTurn and StopSession race.
func KillProcessGroup(pid int) error {
	_, err := killProcessGroupReportingLeftover(pid)
	return err
}

// processAlreadyGone reports whether err, as [os.Process.Kill]
// returned it for a process the reap has already waited for, means the
// process was already gone rather than that the kill could not reach a
// live one.
//
// Kill answers for the two states a waited-for process is left in.
// os/exec marks it done on Unix, which reports [os.ErrProcessDone], and
// releases the handle on Windows, where a successful Wait deliberately
// releases rather than marks done, which reports [syscall.EINVAL] on
// its own. A kill that reached a live process and failed reports an
// [os.SyscallError] naming the call it made instead, so no such failure
// is read as the process having been gone.
func processAlreadyGone(err error) bool {
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}
	if _, ok := errors.AsType[*os.SyscallError](err); ok {
		return false
	}
	return errors.Is(err, syscall.EINVAL)
}

// killProcessGroupReportingLeftover terminates the Job Object
// registered for pid, exactly as [KillProcessGroup] does, and reports
// whether a member other than one already exited was still running
// just before that termination. See the "Leftover report" logic this
// mirrors for the exact membership check.
//
// Termination itself is bounded by [drainJobObject]: it resends until
// jobHasRunningMember finds no member left or groupDrainBound passes,
// mirroring the Unix path's resend-and-wait loop for the same reason.
func killProcessGroupReportingLeftover(pid int) (leftover bool, err error) {
	v, ok := jobs.LoadAndDelete(pid)
	if !ok {
		return false, nil
	}
	entry := v.(*jobEntry)
	if entry.job == 0 {
		if entry.proc == nil {
			return false, nil
		}
		// The reap waits for the direct child before it gets here, so the
		// kill almost always finds it already gone. That is the outcome
		// the kill was asked for, not a failure to reach it, and
		// reporting it would raise the cleanup warning on every launch
		// that ran without a Job Object. The Unix path maps ESRCH to nil
		// for the same reason.
		if killErr := entry.proc.Kill(); killErr != nil && !processAlreadyGone(killErr) {
			return false, killErr
		}
		return false, nil
	}

	// A member list that could not be read leaves the leftover report
	// with nothing to report: the INFO record it feeds names processes
	// this teardown observed alive, so a launch that left none behind
	// must not raise it on a query that failed. The drain below reports
	// that same failure as a teardown it could not confirm.
	leftover, _ = jobHasRunningMember(entry.job)
	err = drainJobObject(pid, entry.job)
	// A failing CloseHandle means the handle was already invalid, which
	// leaves the caller nothing to act on; the drain result is the one
	// worth reporting.
	_ = windows.CloseHandle(entry.job)
	return leftover, err
}

// drainJobObject terminates job repeatedly, on every poll so a member
// gained after the first termination does not outlive it, until
// jobHasRunningMember reports none left or groupDrainBound passes. It
// shares that bound, the poll interval, and the termination call with
// [runJobDrain]'s own drain, but settles on jobHasRunningMember's
// console-host exclusion and liveness rule rather than the job's raw
// active-process count, matching the membership check
// killProcessGroupReportingLeftover already used for the leftover
// report.
//
// A non-nil error means a termination call itself failed, or the job
// still held a running member when the bound ran out, or its member
// list could not be read for the whole of that bound; either way a
// descendant may have survived the reap. Only a member list that was
// read and held no running member returns nil, so a query that fails
// leaves the job unconfirmed rather than drained.
func drainJobObject(pid int, job windows.Handle) error {
	deadline := time.Now().Add(groupDrainBound)
	for {
		if termErr := terminateJobObjectFunc(job, jobTerminateExitCode); termErr != nil {
			return termErr
		}
		running, queryErr := jobHasRunningMember(job)
		if queryErr == nil && !running {
			return nil
		}
		if !time.Now().Before(deadline) {
			if queryErr != nil {
				return fmt.Errorf("job object for process %d could not be confirmed empty within the %s drain bound: %w", pid, groupDrainBound, queryErr)
			}
			return fmt.Errorf("job object for process %d still had a running member after the %s drain bound", pid, groupDrainBound)
		}
		time.Sleep(groupDrainPollInterval)
	}
}

// assignToJobObject creates an anonymous Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and assigns pid to it. The Job
// Object ensures every process it holds is terminated when its last
// handle closes, preventing orphans even if the orchestrator crashes.
//
// When keepHandle is true, assignToJobObject also duplicates the
// handle so a caller that must drain the job itself later (a Capture)
// holds a reference independent of the one [registerJobAssignment]
// stores; dup is zero when keepHandle is false or the duplicate
// failed, in which case the caller drains nothing of its own but the
// job is still assigned and registered normally.
//
// Must be called after [exec.Cmd.Start]. On failure job and dup are
// both zero; callers should log at WARN, register the process without
// a job through [registerJobAssignment], and continue.
func assignToJobObject(pid int, keepHandle bool) (job, dup windows.Handle, err error) {
	processID, err := dwordPID(pid)
	if err != nil {
		return 0, 0, err
	}

	job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("CreateJobObject: %w", err)
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
		return 0, 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		processID,
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, 0, fmt.Errorf("OpenProcess: %w", err)
	}

	err = windows.AssignProcessToJobObject(job, processHandle)
	_ = windows.CloseHandle(processHandle)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, 0, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	if !keepHandle {
		return job, 0, nil
	}

	self := windows.CurrentProcess()
	if dupErr := windows.DuplicateHandle(self, job, self, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); dupErr != nil {
		return job, 0, nil
	}
	return job, dup, nil
}

// registerJobAssignment records proc's Job Object membership so
// [KillProcessGroup] and [CleanupProcess] can reach it. job is zero
// when Job Object assignment failed, which leaves KillProcessGroup
// falling back to proc.Kill.
func registerJobAssignment(pid int, proc *os.Process, job windows.Handle) {
	jobs.Store(pid, &jobEntry{job: job, proc: proc})
}

// CleanupProcess releases the Job Object handle associated with pid.
// Safe to call multiple times or with an unregistered PID.
func CleanupProcess(pid int) {
	v, ok := jobs.LoadAndDelete(pid)
	if ok {
		entry := v.(*jobEntry)
		if entry.job != 0 {
			_ = windows.CloseHandle(entry.job)
		}
	}
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
	NumberOfProcessIDsInList  uint32
	ProcessIDList             [1]uintptr
}

// maxJobMembers bounds the number of entries jobMemberPIDs reads from
// the trailing variable-length array; a launch's process tree is
// small, so this is a generous ceiling rather than a measured one.
const maxJobMembers = 1024

// jobMemberPIDs returns the PIDs currently associated with job.
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
	count := min(list.NumberOfProcessIDsInList, maxJobMembers)

	first := unsafe.Pointer(&list.ProcessIDList[0]) //nolint:gosec // G103: base address of the over-allocated trailing array
	pids := make([]uint32, 0, count)
	for i := range count {
		entry := (*uintptr)(unsafe.Add(first, uintptr(i)*unsafe.Sizeof(uintptr(0)))) //nolint:gosec // G103: indexing past the fixed one-element array bound into the over-allocated buffer
		pids = append(pids, uint32(*entry))                                          //nolint:gosec // G115: a Windows PID never exceeds uint32 range
	}
	return pids, nil
}

// procIsProcessInJob binds the kernel32 export IsProcessInJob, which
// golang.org/x/sys/windows v0.48.0 does not bind.
var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// isProcessInJob reports whether process belongs to job.
func isProcessInJob(process, job windows.Handle) (bool, error) {
	var result int32
	ret, _, callErr := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result))) //nolint:gosec // G103: IsProcessInJob's PBOOL out-parameter has no typed x/sys/windows alternative
	if ret == 0 {
		return false, callErr
	}
	return result != 0, nil
}

// queryImageBaseName returns the base name of the executable process
// (opened with at least PROCESS_QUERY_LIMITED_INFORMATION) is running.
func queryImageBaseName(process windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(windows.MAX_PATH)
	if err := windows.QueryFullProcessImageName(process, 0, &buf[0], &size); err != nil {
		return "", fmt.Errorf("QueryFullProcessImageName: %w", err)
	}
	return filepath.Base(windows.UTF16ToString(buf[:size])), nil
}

// jobHasRunningMember reports whether job's process identifier list
// holds a process, other than a console host, that IsProcessInJob
// still places in the job and that a zero-timeout WaitForSingleObject
// finds running. A process the list names but that has already exited
// (including the reap's own direct child) is signaled rather than
// timed out, and does not count.
//
// A non-nil error means the member list itself could not be read, so
// neither answer is established: false then means the job was not
// observed to hold a running member, never that it was observed empty.
// Only a caller that may treat an unread list the way it treats a
// confirmed-empty one may discard the error.
func jobHasRunningMember(job windows.Handle) (bool, error) {
	pids, err := jobMemberPIDs(job)
	if err != nil {
		return false, err
	}
	for _, pid := range pids {
		if memberIsRunning(job, pid) {
			return true, nil
		}
	}
	return false, nil
}

// isConsoleHostImage reports whether name, a process image base name,
// names the Windows console host. Windows starts one for a client with
// no console to inherit, so it is never a process an operator's own
// command started, whatever started it.
func isConsoleHostImage(name string) bool {
	return strings.EqualFold(name, "conhost.exe")
}

// memberIsRunning reports whether pid, read from job's own member
// list, is still a running, non-console-host member of job. Confirming
// membership through IsProcessInJob after opening the PID by number
// guards against the identifier having been recycled between the list
// read and this check.
func memberIsRunning(job windows.Handle, pid uint32) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	if inJob, err := isProcessInJob(handle, job); err != nil || !inJob {
		return false
	}

	if image, err := queryImageBaseName(handle); err == nil && isConsoleHostImage(image) {
		return false
	}

	// Read last, after the name query rather than before it: a member
	// that exits between opening its handle and here can make
	// queryImageBaseName fail with no console host ever having been
	// named, and a query failure must not default to counting the
	// member. Its current state, not a name that could not be read,
	// decides whether it still counts; WaitForSingleObject signals a
	// process handle exactly when the process terminates, so a query
	// failure caused by the same exit is resolved by this same check
	// finding the handle already signaled.
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}
