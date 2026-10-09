//go:build windows

package procutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
// registers its launch record, starts it, and assigns and resumes it.
// ctx is the context cmd was created with; it decides the resume.
// keepJobHandle requests a duplicate Job Object handle for a caller
// that drains the job itself later (a Capture); the returned handle is
// zero when keepJobHandle is false, when assignment failed, or when
// Unix has no Job Object analogue.
//
// The returned record is nil on every error. A [StageProcessStart]
// error means cmd.Start failed. A [StageProcessResume] error means the
// process started but was not resumed: either ctx was done when the
// resume was reached, which is read once before the resume call and
// reported as Cancelled with ctx's error, or the resume itself failed.
// Either way the process never ran code; by the time startAndAssign
// returns, its direct child has exited or groupDrainBound has elapsed,
// and the reap is left to a reaper goroutine nobody awaits here.
//
// startedAt is the moment cmd.Start returned, for a caller that later
// drains the job and needs it for the root probe's PID-reuse guard;
// it is the zero value when cmd.Start failed.
func startAndAssign(ctx context.Context, cmd *exec.Cmd, logger *slog.Logger, keepJobHandle bool) (g *Group, jobHandle uintptr, startedAt time.Time, startErr *StartError) {
	SetProcessGroup(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED

	// os/exec may run Cancel as soon as Start returns, and a cancellation
	// that found no record would skip the group.
	g = newGroup(cmd)
	groups.Store(cmd, g)
	recordCancelStop(cmd, g)

	if err := cmd.Start(); err != nil {
		groups.Delete(cmd)
		return nil, 0, time.Time{}, processStartError(err)
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
	g.finishAssignment(job)

	resumeSeam()

	// ctx is read once, before the resume call: a deadline that expires
	// during the call leaves the launch resumed. Holding the lock orders
	// the read against every stop, which also takes it, and nothing that
	// terminates or waits may run under it because the cancellation hook
	// that abandonUnresumed waits on needs it.
	g.mu.Lock()
	cause := ctx.Err()
	jobForDrain := g.job
	var resumeErr error
	if cause == nil {
		resumeErr = resumeProcess(cmd.Process.Pid)
	}
	g.mu.Unlock()

	if cause == nil && resumeErr == nil {
		return g, uintptr(dup), startedAt, nil
	}
	if resumeErr != nil {
		logger.Warn("process resume failed",
			slog.String("command", filepath.Base(cmd.Path)),
			slog.String("dir", cmd.Dir),
			slog.Any("error", resumeErr))
	}

	abandonUnresumed(cmd, jobForDrain, dup, logger)
	if cause != nil {
		return nil, 0, startedAt, &StartError{Stage: StageProcessResume, Err: cause, Cancelled: true}
	}
	return nil, 0, startedAt, &StartError{Stage: StageProcessResume, Err: resumeErr}
}

// abandonUnresumed terminates the process of a launch that was started
// suspended and will not be resumed, and hands its reap to a reaper
// goroutine it does not await: the reap can be stuck on os/exec's copy
// of a caller-owned stdin reader. The termination completes before the
// reaper starts, because the reaper closes the Job Object handle job
// names. It returns once the direct child exited or groupDrainBound
// elapsed, so a caller that removes the working directory next finds no
// handle of the refused process open in it. It closes dup, the
// capture's duplicate of the job handle, when non-zero. It MUST be
// called holding no lock.
func abandonUnresumed(cmd *exec.Cmd, job windows.Handle, dup windows.Handle, logger *slog.Logger) {
	if job != 0 {
		_ = drainJobObject(cmd.Process.Pid, job)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Process.WithHandle(func(handle uintptr) {
		_, _ = windows.WaitForSingleObject(windows.Handle(handle), uint32(groupDrainBound.Milliseconds())) //nolint:gosec // G115: groupDrainBound is a small constant-scale duration
	})
	StartReaper(cmd, logger)
	if dup != 0 {
		_ = windows.CloseHandle(dup)
	}
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
	Image     string // base name from the system process snapshot
	InJob     bool   // PID present in jobTeardown.JobMemberPIDs
}

// unexaminedCandidate names one process attributed to a capture's direct
// child whose state the survivor scan could not read, so it is neither a
// confirmed survivor nor confirmed gone. Reason is text because slog's
// JSON handler keeps only the message of an error it is handed.
type unexaminedCandidate struct {
	PID       uint32
	ParentPID uint32
	Image     string
	InJob     bool   // PID present in jobTeardown.JobMemberPIDs
	Reason    string // text of the failed open, creation-time read, or wait
}

// processEntry is one process of the system snapshot.
type processEntry struct {
	pid, parent uint32
	created     time.Time
	image       string
}

// processLifetime is the direct child's creation and exit time, read from
// its wait status.
type processLifetime struct {
	created, exited time.Time
}

// jobTeardown carries what drainCaptureJob observed while tearing a
// Windows launch's process tree down, for one launch. Every field is
// populated on every path that reaches the point where the record is
// emitted, except that Survivors, Unexamined and SurvivorScanErr stay
// empty when no survivor scan ran.
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
	Unexamined        []unexaminedCandidate
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
// measured. cmd MUST have been waited for: the scan attributes
// processes by the direct child's lifetime in cmd.ProcessState, and
// declines, recording why in the teardown record, when that lifetime is
// unreadable.
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
		lifetime, lifetimeErr := directChildLifetime(cmd.ProcessState)
		if lifetimeErr != nil {
			teardown.SurvivorScanErr = lifetimeErr
		} else {
			teardown.Survivors, teardown.Unexamined, teardown.SurvivorScanErr = scanSurvivorsFunc(rootPID, teardown.JobMemberPIDs, lifetime)
		}
	}

	logJobTeardown(logger, teardown)
}

// logJobTeardown emits exactly one record per drainCaptureJob call
// describing what teardown observed. The level is Warn when the tree is
// not shown to be gone: a surviving descendant, a candidate the scan
// could not examine, a non-zero active-process count at the last drain
// poll, a failed or declined survivor scan, a failed job-member-list
// read, or a failed drain query; otherwise it is Debug, which the
// default logger suppresses. DrainTerminateErr never raises the level
// on its own: a job that drained despite a failed termination left no
// process, and one that did not drain already takes the Warn arm
// through ActiveLast. RootProbeErr never raises it either.
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
		slog.Any("unexamined", teardown.Unexamined),
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
		len(teardown.Unexamined) > 0 ||
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

const (
	snapshotInitialSize = 1 << 20
	snapshotGrowSlack   = 64 << 10
	snapshotMaxCalls    = 3
)

// filetimeTime converts a FILETIME the way [windows.Filetime.Nanoseconds]
// does, so every creation and exit time compared in this package shares
// one conversion rather than raw tick arithmetic.
func filetimeTime(low, high uint32) time.Time {
	ft := windows.Filetime{LowDateTime: low, HighDateTime: high}
	return time.Unix(0, ft.Nanoseconds())
}

// processCreated returns the creation time of the process handle names.
// handle needs PROCESS_QUERY_LIMITED_INFORMATION.
func processCreated(handle windows.Handle) (time.Time, error) {
	var creationTime, exitTime, kernelTime, userTime windows.Filetime
	if err := windows.GetProcessTimes(handle, &creationTime, &exitTime, &kernelTime, &userTime); err != nil {
		return time.Time{}, fmt.Errorf("GetProcessTimes: %w", err)
	}
	return filetimeTime(creationTime.LowDateTime, creationTime.HighDateTime), nil
}

// processSnapshot lists every process of the system with its parent and
// creation time, without opening any of them: an unopenable process such
// as CSRSS is listed like any other.
func processSnapshot() ([]processEntry, error) {
	size := uint32(snapshotInitialSize)
	for range snapshotMaxCalls {
		// A uint64 backing array keeps the buffer 8-byte aligned for the
		// structures read back out of it.
		buf := make([]uint64, (size+7)/8)
		var needed uint32
		status := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &needed) //nolint:gosec // G103: NtQuerySystemInformation takes its output buffer as an unsafe.Pointer
		if errors.Is(status, windows.STATUS_INFO_LENGTH_MISMATCH) {
			size = needed + snapshotGrowSlack
			continue
		}
		if status != nil {
			return nil, fmt.Errorf("NtQuerySystemInformation: %w", status)
		}
		return readProcessEntries(unsafe.Pointer(&buf[0])), nil //nolint:gosec // G103: reading the kernel-filled buffer back as the mirrored structures
	}
	return nil, errors.New("NtQuerySystemInformation: the process list kept growing between calls")
}

// readProcessEntries copies every entry out of a filled
// SystemProcessInformation buffer, because the image name of each points
// into it.
func readProcessEntries(base unsafe.Pointer) []processEntry {
	var entries []processEntry
	for offset := uintptr(0); ; {
		info := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Add(base, offset)) //nolint:gosec // G103: indexing entries by NextEntryOffset inside the kernel-filled buffer
		var image string
		if info.ImageName.Buffer != nil {
			image = windows.UTF16ToString(unsafe.Slice(info.ImageName.Buffer, info.ImageName.Length/2)) //nolint:gosec // G103: the image name is a counted UTF-16 string inside the buffer
		}
		entries = append(entries, processEntry{
			pid:     uint32(info.UniqueProcessID),                                       //nolint:gosec // G115: a Windows PID never exceeds uint32 range
			parent:  uint32(info.InheritedFromUniqueProcessID),                          //nolint:gosec // G115: a Windows PID never exceeds uint32 range
			created: filetimeTime(uint32(info.CreateTime), uint32(info.CreateTime>>32)), //nolint:gosec // G115: splitting a FILETIME into its two 32-bit halves
			image:   image,
		})
		if info.NextEntryOffset == 0 {
			return entries
		}
		offset += uintptr(info.NextEntryOffset)
	}
}

// directChildLifetime reads the creation and exit time of the process
// state describes from the wait status os/exec recorded, so it needs no
// handle and cannot be misled by a recycled identifier. It returns an
// error when state carries no readable lifetime.
func directChildLifetime(state *os.ProcessState) (processLifetime, error) {
	if state == nil {
		return processLifetime{}, errors.New("direct child lifetime: no wait status")
	}
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage == nil {
		return processLifetime{}, fmt.Errorf("direct child lifetime: unexpected resource usage type %T", state.SysUsage())
	}
	lifetime := processLifetime{
		created: filetimeTime(usage.CreationTime.LowDateTime, usage.CreationTime.HighDateTime),
		exited:  filetimeTime(usage.ExitTime.LowDateTime, usage.ExitTime.HighDateTime),
	}
	if lifetime.exited.Before(lifetime.created) {
		return processLifetime{}, errors.New("direct child lifetime: exit time precedes creation time")
	}
	return lifetime, nil
}

// descendantEntries returns the entries reachable from the direct child
// rootPID, whose lifetime is root. Each edge is bounded by the lifetime of
// the parent it leaves: a child created before that parent, or after its
// exit, belongs to another holder of the identifier and is dropped with
// everything it reaches. A child created exactly at either bound is kept.
// An entry holding rootPID is a candidate like any other.
func descendantEntries(entries []processEntry, rootPID uint32, root processLifetime) []processEntry {
	type lineage struct {
		pid uint32
		processLifetime
	}
	frontier := []lineage{{pid: rootPID, processLifetime: root}}
	visited := make(map[uint32]bool)
	var found []processEntry
	for len(frontier) > 0 {
		parent := frontier[0]
		frontier = frontier[1:]
		for _, e := range entries {
			if e.parent != parent.pid || visited[e.pid] {
				continue
			}
			if e.created.Before(parent.created) {
				continue
			}
			if !parent.exited.IsZero() && e.created.After(parent.exited) {
				continue
			}
			visited[e.pid] = true
			found = append(found, e)
			frontier = append(frontier, lineage{pid: e.pid, processLifetime: processLifetime{created: e.created}})
		}
	}
	return found
}

// scanSurvivors classifies every process attributed to the direct child
// rootPID, whose lifetime is root, after the drain returned. A candidate
// still running is a survivor; one that could not be read, whether its
// open, creation-time read, or wait failed, is unexamined and MUST NOT be
// treated as gone; one confirmed exited, or whose identifier a different
// process now holds, is dropped. A console host is dropped by image base
// name. The error is non-nil only when the snapshot itself failed.
func scanSurvivors(rootPID uint32, jobPIDs []uint32, root processLifetime) ([]jobSurvivor, []unexaminedCandidate, error) {
	entries, err := processSnapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("process snapshot: %w", err)
	}

	survivors := make([]jobSurvivor, 0)
	unexamined := make([]unexaminedCandidate, 0)
	for _, e := range descendantEntries(entries, rootPID, root) {
		if isConsoleHostImage(e.image) {
			continue
		}
		inJob := slices.Contains(jobPIDs, e.pid)
		running, runErr := processRunning(e.pid, func(handle windows.Handle) (bool, error) {
			created, createdErr := processCreated(handle)
			if createdErr != nil {
				return false, createdErr
			}
			return created.Equal(e.created), nil
		})
		switch {
		case runErr != nil:
			unexamined = append(unexamined, unexaminedCandidate{
				PID:       e.pid,
				ParentPID: e.parent,
				Image:     e.image,
				InJob:     inJob,
				Reason:    runErr.Error(),
			})
		case running:
			survivors = append(survivors, jobSurvivor{
				PID:       e.pid,
				ParentPID: e.parent,
				Image:     e.image,
				InJob:     inJob,
			})
		}
	}
	return survivors, unexamined, nil
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

	created, createdErr := processCreated(handle)
	if createdErr != nil {
		return inList, false, createdErr
	}

	if created.After(startedAt) {
		// Created after the direct child was started, so the PID was
		// recycled; this is not the direct child.
		return inList, false, nil
	}
	return inList, true, nil
}
