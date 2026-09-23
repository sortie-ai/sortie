//go:build windows

package procutil

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// assignSeam runs between a Windows launch's process start and its Job
// Object creation, reproducing on demand a start-to-assign race that
// production code never deliberately introduces. Its zero value is a
// no-op; only a test replaces it.
var assignSeam = func() {}

// resumeSeam runs immediately before a Windows launch's resume, giving
// a test a place to delay or cancel the launch at the last moment
// before its suspended process would otherwise run. Its zero value is
// a no-op; only a test replaces it.
var resumeSeam = func() {}

// resumeProcess is the Windows resume call: it opens pid with
// PROCESS_SUSPEND_RESUME and resumes it through [ntResumeProcess]. Only
// a test replaces it, to inject a resume failure with no dependency on
// a real race.
var resumeProcess = defaultResumeProcess

// assignToJobObjectFunc is startAndAssign's Job Object assignment call.
// Only a test replaces it, to inject an assignment failure with no
// dependency on a real Windows failure condition, exercising the
// fail-open path: one WARN record logged and the process still
// resumed.
var assignToJobObjectFunc = assignToJobObject

// ntdll and procNtResumeProcess bind the ntdll.dll export NtResumeProcess,
// which golang.org/x/sys/windows v0.48.0 does not bind, through the
// package's own NewLazySystemDLL/NewProc pattern rather than CGo.
var (
	ntdll               = windows.NewLazySystemDLL("ntdll.dll")
	procNtResumeProcess = ntdll.NewProc("NtResumeProcess")
)

// ntResumeProcess resumes every thread of the process process refers to,
// undoing the suspension CREATE_SUSPENDED left it in. NTSTATUS values
// are failures when their high bit is set.
func ntResumeProcess(process windows.Handle) error {
	r1, _, _ := procNtResumeProcess.Call(uintptr(process))
	if int32(r1) < 0 { //nolint:gosec // G115: NTSTATUS is inspected as a signed 32-bit value by design
		return fmt.Errorf("NtResumeProcess: NTSTATUS %#x", uint32(r1))
	}
	return nil
}

func defaultResumeProcess(pid int) error {
	processID, err := dwordPID(pid)
	if err != nil {
		return err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, processID)
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return ntResumeProcess(handle)
}

// startAndAssign creates cmd suspended within a new process group,
// starts it, and assigns, registers, and resumes it. keepJobHandle
// requests a duplicate Job Object handle for a caller that drains the
// job itself later (a Capture); the returned handle is zero when
// keepJobHandle is false, when assignment failed, or when Unix has no
// Job Object analogue.
//
// A returned error with a nil cmd.Process means cmd.Start failed. Any
// other error means the process started but could not be resumed: by
// the time startAndAssign returns, the process has already been
// killed, reaped, and, when keepJobHandle, had its job drained and its
// teardown record logged.
//
// startedAt is the moment cmd.Start returned, for a caller that later
// drains the job and needs it for the root probe's PID-reuse guard;
// it is the zero value when cmd.Start failed.
func startAndAssign(cmd *exec.Cmd, logger *slog.Logger, keepJobHandle bool) (jobHandle uintptr, startedAt time.Time, err error) {
	SetProcessGroup(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED

	// os/exec may run Cancel as soon as Start returns, and a cancellation
	// that looked up the job before its registration would miss it.
	registered := make(chan struct{})
	if cancel := cmd.Cancel; cancel != nil {
		cmd.Cancel = func() error {
			<-registered
			return cancel()
		}
	}

	if startErr := cmd.Start(); startErr != nil {
		return 0, time.Time{}, startErr
	}
	startedAt = time.Now()

	assignSeam()

	job, dup, assignErr := assignToJobObjectFunc(cmd.Process.Pid, keepJobHandle)
	if assignErr != nil {
		logger.Warn("process group assignment failed",
			slog.String("command", filepath.Base(cmd.Path)),
			slog.String("dir", cmd.Dir),
			slog.Any("error", assignErr))
	}
	registerJobAssignment(cmd.Process.Pid, cmd.Process, job)
	close(registered)

	resumeSeam()

	if resumeErr := resumeProcess(cmd.Process.Pid); resumeErr != nil {
		logger.Warn("process resume failed",
			slog.String("command", filepath.Base(cmd.Path)),
			slog.String("dir", cmd.Dir),
			slog.Any("error", resumeErr))

		_ = cmd.Process.Kill()
		reapStart := time.Now()
		r := StartReaper(cmd, logger)
		<-r.Done()
		waitMS := time.Since(reapStart).Milliseconds()

		if keepJobHandle {
			drainCaptureJob(uintptr(dup), cmd, startedAt, waitMS, logger)
		}
		return 0, startedAt, resumeErr
	}

	return uintptr(dup), startedAt, nil
}

// terminateJobObjectFunc is runJobDrain's termination call. Only a test
// replaces it, to exercise a termination that fails or does nothing.
var terminateJobObjectFunc = windows.TerminateJobObject

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

// jobDrainResult carries what one call to runJobDrain observed.
type jobDrainResult struct {
	DrainMS           int64
	DrainPolls        int
	ActiveFirst       uint32
	ActiveLast        uint32
	TotalProcesses    uint32
	TotalTerminated   uint32
	DrainTerminateErr error
	DrainQueryErr     error
}

// runJobDrain terminates job repeatedly, on every poll so a process
// created after the first termination does not outlive it, until the
// job reports no active process or groupDrainBound passes. Termination
// completes asynchronously, so returning before the active count
// reaches zero would let a dying member briefly outlive the caller with
// its working-directory handle still open.
func runJobDrain(job windows.Handle) jobDrainResult {
	start := time.Now()
	deadline := start.Add(groupDrainBound)

	var result jobDrainResult
	for {
		if termErr := terminateJobObjectFunc(job, jobTerminateExitCode); termErr != nil && result.DrainTerminateErr == nil {
			result.DrainTerminateErr = termErr
		}

		var info jobObjectBasicAccountingInformation
		queryErr := windows.QueryInformationJobObject(
			job,
			windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: QueryInformationJobObject takes the accounting struct as a uintptr; x/sys/windows exposes no typed alternative
			uint32(unsafe.Sizeof(info)),
			nil,
		)
		result.DrainPolls++
		if queryErr != nil {
			result.DrainQueryErr = queryErr
			result.DrainMS = time.Since(start).Milliseconds()
			return result
		}
		if result.DrainPolls == 1 {
			result.ActiveFirst = info.ActiveProcesses
		}
		result.ActiveLast = info.ActiveProcesses
		result.TotalProcesses = info.TotalProcesses
		result.TotalTerminated = info.TotalTerminatedProcesses
		if info.ActiveProcesses == 0 || time.Now().After(deadline) {
			result.DrainMS = time.Since(start).Milliseconds()
			return result
		}
		time.Sleep(groupDrainPollInterval)
	}
}

// jobSurvivor names one process descended from a capture's direct child
// that was still alive after the drain returned.
type jobSurvivor struct {
	PID       uint32
	ParentPID uint32
	Image     string // ProcessEntry32.ExeFile, base name only
	InJob     bool   // PID present in jobTeardown.JobMemberPIDs
}

// jobTeardown carries what drainCaptureJob observed while tearing a
// Windows launch's process tree down, for one launch. Every field is
// populated on every path that reaches the point where the record is
// emitted.
type jobTeardown struct {
	Command           string
	Dir               string
	WaitMS            int64
	DrainMS           int64
	DrainPolls        int
	ActiveFirst       uint32
	ActiveLast        uint32
	TotalProcesses    uint32
	TotalTerminated   uint32
	DrainTerminateErr error
	DrainQueryErr     error
	JobMemberPIDs     []uint32
	JobListErr        error
	RootInJobList     bool
	RootOpenable      bool
	RootProbeErr      error
	Survivors         []jobSurvivor
	SurvivorScanErr   error
}

// scanSurvivorsFunc scans the process list for members of a launch's
// tree still alive after its drain returned. Only a test replaces it.
var scanSurvivorsFunc = scanSurvivors

// drainCaptureJob drains job (skipped when job is zero, meaning
// assignment never produced one), reads its member list and probes the
// direct child identified by cmd.Process, scanning the process list for
// survivors only when there is no Job Object, the drain ended with an
// active process, or a drain query failed, logs the teardown record,
// and closes job. startedAt is the moment cmd.Start returned, for the
// root probe's PID-reuse guard; waitMS is the reap duration the caller
// measured.
func drainCaptureJob(job uintptr, cmd *exec.Cmd, startedAt time.Time, waitMS int64, logger *slog.Logger) {
	jobHandle := windows.Handle(job)
	hasJob := jobHandle != 0
	if hasJob {
		defer func() { _ = windows.CloseHandle(jobHandle) }()
	}

	teardown := jobTeardown{
		Command: filepath.Base(cmd.Path),
		Dir:     cmd.Dir,
		WaitMS:  waitMS,
	}

	if hasJob {
		drain := runJobDrain(jobHandle)
		teardown.DrainMS = drain.DrainMS
		teardown.DrainPolls = drain.DrainPolls
		teardown.ActiveFirst = drain.ActiveFirst
		teardown.ActiveLast = drain.ActiveLast
		teardown.TotalProcesses = drain.TotalProcesses
		teardown.TotalTerminated = drain.TotalTerminated
		teardown.DrainTerminateErr = drain.DrainTerminateErr
		teardown.DrainQueryErr = drain.DrainQueryErr

		teardown.JobMemberPIDs, teardown.JobListErr = jobMemberPIDs(jobHandle)
	}

	rootPID, pidErr := dwordPID(cmd.Process.Pid)
	if pidErr == nil {
		teardown.RootInJobList, teardown.RootOpenable, teardown.RootProbeErr = probeRootProcess(rootPID, teardown.JobMemberPIDs, startedAt)
	}

	needsScan := !hasJob || teardown.ActiveLast > 0 || teardown.DrainQueryErr != nil
	if needsScan && pidErr == nil {
		teardown.Survivors, teardown.SurvivorScanErr = scanSurvivorsFunc(rootPID, teardown.JobMemberPIDs)
	}

	logJobTeardown(logger, teardown)
}

// logJobTeardown emits exactly one record per drainCaptureJob call
// describing what teardown observed. The level is Warn only on
// conclusive evidence of a leak (a surviving descendant, a non-zero
// active-process count at the last drain poll, a failed survivor scan,
// or a failed job-member-list read); otherwise it is Debug, which the
// default logger suppresses. DrainTerminateErr never raises the level
// on its own: a job that drained despite a failed termination left no
// process, and one that did not drain already takes the Warn arm
// through ActiveLast.
func logJobTeardown(logger *slog.Logger, teardown jobTeardown) {
	if logger == nil {
		logger = slog.Default()
	}

	args := []any{
		slog.String("command", teardown.Command),
		slog.String("dir", teardown.Dir),
		slog.Int64("wait_ms", teardown.WaitMS),
		slog.Int64("drain_ms", teardown.DrainMS),
		slog.Int("drain_polls", teardown.DrainPolls),
		slog.Int64("active_first", int64(teardown.ActiveFirst)),
		slog.Int64("active_last", int64(teardown.ActiveLast)),
		slog.Int64("total_processes", int64(teardown.TotalProcesses)),
		slog.Int64("total_terminated", int64(teardown.TotalTerminated)),
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
	if teardown.DrainTerminateErr != nil {
		args = append(args, slog.Any("drain_terminate_err", teardown.DrainTerminateErr))
	}

	unsettled := len(teardown.Survivors) > 0 ||
		teardown.ActiveLast > 0 ||
		teardown.SurvivorScanErr != nil ||
		teardown.JobListErr != nil ||
		teardown.DrainQueryErr != nil
	if unsettled {
		logger.Warn("subprocess tree did not settle", args...)
		return
	}
	logger.Debug("subprocess tree settled", args...)
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

// processIsRunning reports whether pid names a process that is still
// running. Opening the identifier does not settle it on its own: a
// process object stays openable after the process exits, for as long as
// anything still holds a handle to it, so an exited descendant would
// pass that check. A process handle is signaled once the process exits,
// so only a zero-timeout wait that times out means it is still running.
func processIsRunning(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	event, waitErr := windows.WaitForSingleObject(handle, 0)
	return waitErr == nil && event == uint32(windows.WAIT_TIMEOUT)
}

// scanSurvivors reports every process descended from rootPID that is
// still alive after the drain returned. A discovered PID is not pinned
// by any handle of ours, so each candidate is confirmed still running
// before it is reported; a candidate that has exited, or whose
// identifier was recycled, is dropped rather than reported as a
// survivor. A console host is dropped the same way the leftover report
// drops one, by image base name; the snapshot this walk reads it from
// carries the name for every entry it holds, so unlike the leftover
// report's live query this exclusion has no candidate whose name it
// could fail to read.
func scanSurvivors(rootPID uint32, jobPIDs []uint32) ([]jobSurvivor, error) {
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
	// Only ERROR_NO_MORE_FILES ends a completed walk. Any other error
	// stops it partway, and reporting the descendants gathered so far as
	// if they were all of them would let the teardown record call a tree
	// settled while a live descendant sat past the truncation.
	if !errors.Is(walkErr, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("Process32Next: %w", walkErr)
	}

	inJob := make(map[uint32]bool, len(jobPIDs))
	for _, jobPID := range jobPIDs {
		inJob[jobPID] = true
	}

	// Build the transitive closure of PIDs reachable from rootPID by
	// following ParentProcessID edges, breadth-first, excluding rootPID
	// itself.
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

	survivors := make([]jobSurvivor, 0, len(closure))
	for _, e := range entries {
		if !closure[e.pid] {
			continue
		}
		if isConsoleHostImage(e.image) {
			continue
		}
		if !processIsRunning(e.pid) {
			continue
		}
		survivors = append(survivors, jobSurvivor{
			PID:       e.pid,
			ParentPID: e.parent,
			Image:     e.image,
			InJob:     inJob[e.pid],
		})
	}
	return survivors, nil
}

// probeRootProcess reports whether the direct child identified by
// rootPID is still in the job's PID list and whether it is still
// openable after the drain returned. os/exec releases its own handle to
// the child when cmd.Wait returns, so a successful open after the drain
// means the process object outlived both our handle and its removal
// from the job's active count. PID reuse is guarded by comparing the
// opened process's creation time against startedAt, the moment
// cmd.Start returned: the identifier belonged to the direct child while
// it lived, so any later holder of it was created after that moment,
// and a creation time after it means the PID was recycled and is not
// the direct child.
func probeRootProcess(rootPID uint32, jobPIDs []uint32, startedAt time.Time) (inList bool, openable bool, err error) {
	inList = slices.Contains(jobPIDs, rootPID)

	handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, rootPID)
	if openErr != nil {
		// An exited direct child is the expected outcome on most runs.
		return inList, false, nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var creationTime, exitTime, kernelTime, userTime windows.Filetime
	if timesErr := windows.GetProcessTimes(handle, &creationTime, &exitTime, &kernelTime, &userTime); timesErr != nil {
		return inList, false, fmt.Errorf("GetProcessTimes: %w", timesErr)
	}

	// Filetime.Nanoseconds already rebases off the Windows 1601 epoch and
	// returns Unix-epoch nanoseconds, so time.Unix(0, ...) is its exact
	// inverse rather than a raw 100-nanosecond tick count.
	if time.Unix(0, creationTime.Nanoseconds()).After(startedAt) {
		// Created after the direct child was started, so the PID was
		// recycled; this is not the direct child.
		return inList, false, nil
	}
	return inList, true, nil
}
