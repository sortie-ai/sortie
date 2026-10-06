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
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SetProcessGroup configures cmd to start in a new console process group.
// It must be called before [exec.Cmd.Start].
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

func dwordPID(pid int) (uint32, error) {
	if pid < 0 || pid > math.MaxUint32 {
		return 0, fmt.Errorf("process id %d is outside the windows process identifier range", pid)
	}
	return uint32(pid), nil
}

// STATUS_CONTROL_C_EXIT (0xC000013A) is the exit status a Job Object
// termination gives its members, which is how a launch's own wait
// status tells a termination apart from an ordinary non-zero exit.
const jobTerminateExitCode uint32 = 0xC000013A

// processAlreadyGone answers for the two states a waited-for process is
// left in. os/exec marks it done on Unix, which reports
// [os.ErrProcessDone], and releases the handle on Windows, where a
// successful Wait deliberately releases rather than marks done, which
// reports [syscall.EINVAL] on its own. A kill that reached a live process and failed reports an
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

// x/sys/windows defines the information class constant but not the
// JOBOBJECT_BASIC_PROCESS_ID_LIST structure.
type jobObjectBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIDsInList  uint32
	ProcessIDList             [1]uintptr
}

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
	count := min(list.NumberOfProcessIDsInList, maxJobMembers)

	first := unsafe.Pointer(&list.ProcessIDList[0]) //nolint:gosec // G103: base address of the over-allocated trailing array
	pids := make([]uint32, 0, count)
	for i := range count {
		entry := (*uintptr)(unsafe.Add(first, uintptr(i)*unsafe.Sizeof(uintptr(0)))) //nolint:gosec // G103: indexing past the fixed one-element array bound into the over-allocated buffer
		pids = append(pids, uint32(*entry))                                          //nolint:gosec // G115: a Windows PID never exceeds uint32 range
	}
	return pids, nil
}

// x/sys/windows v0.48.0 does not bind IsProcessInJob.
var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func isProcessInJob(process, job windows.Handle) (bool, error) {
	var result int32
	ret, _, callErr := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result))) //nolint:gosec // G103: IsProcessInJob's PBOOL out-parameter has no typed x/sys/windows alternative
	if ret == 0 {
		return false, callErr
	}
	return result != 0, nil
}

// x/sys/windows v0.48.0 does not bind GetProcessImageFileNameW.
var procGetProcessImageFileName = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessImageFileNameW")

// Not QueryFullProcessImageName: it fails for a process still exiting, so
// an exiting console host would go unnamed and count as running.
func queryImageBaseName(process windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	n, _, callErr := procGetProcessImageFileName.Call(uintptr(process), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))) //nolint:gosec // G103: GetProcessImageFileNameW's LPWSTR out-parameter has no typed x/sys/windows alternative
	if n == 0 {
		return "", fmt.Errorf("GetProcessImageFileName: %w", callErr)
	}
	return filepath.Base(windows.UTF16ToString(buf[:n])), nil
}

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

func isConsoleHostImage(name string) bool {
	return strings.EqualFold(name, "conhost.exe")
}

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

	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}
