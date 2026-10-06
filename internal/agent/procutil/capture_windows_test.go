//go:build windows

package procutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func uintptrOf(v any) uintptr {
	switch p := v.(type) {
	case *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION:
		return uintptr(unsafe.Pointer(p)) //nolint:gosec // G103: matches production's own QueryInformationJobObject usage
	case *jobObjectBasicAccountingInformation:
		return uintptr(unsafe.Pointer(p)) //nolint:gosec // G103: matches production's own QueryInformationJobObject usage
	default:
		panic("uintptrOf: unsupported type")
	}
}

func uint32Sizeof(v any) uint32 {
	switch v.(type) {
	case windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION:
		return uint32(unsafe.Sizeof(windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}))
	case jobObjectBasicAccountingInformation:
		return uint32(unsafe.Sizeof(jobObjectBasicAccountingInformation{}))
	default:
		panic("uint32Sizeof: unsupported type")
	}
}

func suspendedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
}

func init() {
	fakeScenarios["procutil.capture-win-leader"] = agenttest.Typed(runCaptureWinLeader)
}

type captureWinLeaderParams struct {
	ChildPath    string
	ChildPIDPath string
	Stdout       string
	ExitCode     int
}

func runCaptureWinLeader(_ []string, p captureWinLeaderParams) int {
	if p.ChildPath != "" {
		cmd := exec.Command(p.ChildPath) //nolint:gosec // fake runtime path this scenario was handed
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "capture win leader: start child: %v\n", err)
			return 2
		}
		if p.ChildPIDPath != "" {
			if err := os.WriteFile(p.ChildPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "capture win leader: write child pid: %v\n", err)
				return 2
			}
		}
	}
	fmt.Print(p.Stdout)
	return p.ExitCode
}

func jobHandleOf(g *Group) windows.Handle {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.job
}

func closeJobHandle(g *Group) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}

func pollCaptureWinPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pollCaptureWinPIDFile(%q): no valid pid after %v", path, timeout)
	return 0
}

func assertCaptureWinProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	processID, convErr := dwordPID(pid)
	if convErr != nil {
		t.Fatalf("dwordPID(%d) error = %v", pid, convErr)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, processID)
		if err != nil {
			return
		}
		event, waitErr := windows.WaitForSingleObject(handle, 0)
		_ = windows.CloseHandle(handle)
		if waitErr == nil && event == uint32(windows.WAIT_OBJECT_0) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d still running after %v, want gone", pid, timeout)
}

func waitCaptureWinResult(t *testing.T, c *Capture, timeout time.Duration) CaptureResult {
	t.Helper()
	done := make(chan CaptureResult, 1)
	go func() { done <- c.Wait() }()
	select {
	case result := <-done:
		return result
	case <-time.After(timeout):
		t.Fatalf("Capture.Wait() did not return within %v", timeout)
		return CaptureResult{}
	}
}

type captureWinLogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

type captureWinLogSpy struct {
	mu      sync.Mutex
	records []captureWinLogRecord
}

func (s *captureWinLogSpy) Enabled(context.Context, slog.Level) bool { return true }

func (s *captureWinLogSpy) Handle(_ context.Context, r slog.Record) error {
	rec := captureWinLogRecord{Level: r.Level, Msg: r.Message, Attrs: make(map[string]slog.Value, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.mu.Unlock()
	return nil
}

func (s *captureWinLogSpy) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *captureWinLogSpy) WithGroup(_ string) slog.Handler      { return s }

func (s *captureWinLogSpy) snapshot() []captureWinLogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]captureWinLogRecord, len(s.records))
	copy(out, s.records)
	return out
}

func latestCaptureTeardownRecord(spy *captureWinLogSpy, from int) (captureWinLogRecord, bool) {
	records := spy.snapshot()
	for i := len(records) - 1; i >= from; i-- {
		if records[i].Msg == "subprocess tree did not settle" || records[i].Msg == "subprocess tree settled" {
			return records[i], true
		}
	}
	return captureWinLogRecord{}, false
}

func TestCapture_HeldDescendantInheritedJobMembership(t *testing.T) {
	dir := t.TempDir()
	childPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-win-leader", captureWinLeaderParams{
		ChildPath:    childPath,
		ChildPIDPath: pidPath,
		Stdout:       "leader output\n",
		ExitCode:     0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	var stdout bytes.Buffer

	start := time.Now()
	result, err := RunCapture(cmd, DefaultStopGrace, CaptureParams{Stdout: &stdout})
	if err != nil {
		t.Fatalf("RunCapture() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("RunCapture() took %v, want within 3s", elapsed)
	}
	if result.WaitErr != nil {
		t.Errorf("WaitErr = %v, want nil", result.WaitErr)
	}
	if !result.OutputComplete {
		t.Error("OutputComplete = false, want true")
	}
	if got := stdout.String(); got != "leader output\n" {
		t.Errorf("collected stdout = %q, want %q", got, "leader output\n")
	}

	childPID := pollCaptureWinPIDFile(t, pidPath, 5*time.Second)
	assertCaptureWinProcessGone(t, childPID, 3*time.Second)
}

func TestCapture_AssignSeamDelayDoesNotLowerJobMembership(t *testing.T) {
	spy := &captureWinLogSpy{}
	logger := slog.New(spy)

	runOnce := func(t *testing.T) int64 {
		t.Helper()
		dir := t.TempDir()
		before := len(spy.snapshot())
		cmd := exec.Command("cmd.exe", "/C", "start /b cmd.exe /C \"ping -n 30 127.0.0.1 >NUL\" & exit 0") //nolint:gosec // fixed literal script
		cmd.Dir = dir
		c, err := StartCapture(cmd, CaptureParams{Logger: logger})
		if err != nil {
			t.Fatalf("StartCapture() error = %v", err)
		}
		_ = waitCaptureWinResult(t, c, 10*time.Second)

		record, ok := latestCaptureTeardownRecord(spy, before)
		if !ok {
			t.Fatalf("no teardown record captured")
		}
		total, ok := record.Attrs["total_processes"]
		if !ok {
			t.Fatalf("teardown record missing total_processes")
		}
		return total.Int64()
	}

	origSeam := assignSeam
	t.Cleanup(func() { assignSeam = origSeam })

	assignSeam = func() {}
	var clean int64
	for range 3 {
		if v := runOnce(t); v > clean {
			clean = v
		}
	}

	assignSeam = func() { time.Sleep(50 * time.Millisecond) }
	delayed := runOnce(t)

	if delayed < clean {
		t.Errorf("total_processes with the seam delay = %d, want >= the clean reference %d", delayed, clean)
	}
}

func TestStartWithOwnedPipes_AssignSeamDelayDoesNotLowerJobMembership(t *testing.T) {
	runOnce := func(t *testing.T) int64 {
		t.Helper()
		dir := t.TempDir()
		cmd := exec.Command("cmd.exe", "/C", "start /b cmd.exe /C \"ping -n 30 127.0.0.1 >NUL\" & exit 0") //nolint:gosec // fixed literal script
		cmd.Dir = dir
		pipes, g := startOwned(t, cmd)
		defer pipes.Close() //nolint:errcheck // best-effort

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("cmd.Wait() did not return within 10s")
		}

		var total int64
		if job := jobHandleOf(g); job != 0 {
			var info jobObjectBasicAccountingInformation
			_ = windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
				uintptrOf(&info), uint32Sizeof(info), nil)
			total = int64(info.TotalProcesses)
		}
		_ = g.Kill()
		closeJobHandle(g)
		return total
	}

	origSeam := assignSeam
	t.Cleanup(func() { assignSeam = origSeam })

	assignSeam = func() {}
	var clean int64
	for range 3 {
		if v := runOnce(t); v > clean {
			clean = v
		}
	}

	assignSeam = func() { time.Sleep(50 * time.Millisecond) }
	delayed := runOnce(t)

	if delayed < clean {
		t.Errorf("registered job's total_processes with the seam delay = %d, want >= the clean reference %d", delayed, clean)
	}
}

func TestStartWithOwnedPipes_CancelBeforeJobRegistrationArmsEscalation(t *testing.T) {
	leaderPath := agenttest.FakeRuntime(t, t.TempDir(), "leader", agenttest.OutputScenario, agenttest.Output{Hang: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupCancel(cmd, escalationGrace)
	groupCancel := cmd.Cancel
	cancelReturned := make(chan struct{})
	cmd.Cancel = func() error {
		defer close(cancelReturned)
		return groupCancel()
	}

	origAssignSeam, origResumeSeam := assignSeam, resumeSeam
	t.Cleanup(func() { assignSeam, resumeSeam = origAssignSeam, origResumeSeam })
	var cancelReturnedBeforeAssignment bool
	assignSeam = func() {
		cancel()
		select {
		case <-cancelReturned:
			cancelReturnedBeforeAssignment = true
		case <-time.After(time.Second):
		}
	}

	// The graceful signal reaches the leader as it starts, before its
	// runtime can catch one, so the leader may exit without starting a
	// descendant. The job gets a member of the test's own instead, added
	// while the leader is still suspended.
	var member *exec.Cmd
	resumeSeam = func() {
		g := lookupGroup(cmd)
		if g == nil {
			t.Fatal("no record registered before the resume, want the launch's record")
		}
		job := jobHandleOf(g)
		if job == 0 {
			t.Fatal("record holds no job before the resume, want the launch's job")
		}
		member = newCaptureTestHeldMember(t, job)
	}

	pipes, group := startOwned(t, cmd)
	defer func() { _ = pipes.Close() }()
	t.Cleanup(func() { _ = group.Kill() })

	if cancelReturnedBeforeAssignment {
		t.Error("the Cancel closure returned before the Job Object assignment finished, want it held at the assignment gate")
	}

	_ = cmd.Wait() //nolint:errcheck // a cancelled command reports the cancellation, not a fault

	memberPID, err := dwordPID(member.Process.Pid)
	if err != nil {
		t.Fatalf("dwordPID() error = %v", err)
	}
	wait := escalationGrace + groupDrainBound + 2*time.Second
	deadline := time.Now().Add(wait)
	for processIsRunning(memberPID) {
		if !time.Now().Before(deadline) {
			t.Fatalf("job member %d still running %v after a cancellation that preceded job registration, want gone", memberPID, wait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunJobDrain(t *testing.T) {
	t.Run("live process needs multiple polls", func(t *testing.T) {
		job, cleanup := newCaptureTestJob(t)
		defer cleanup()
		startCaptureTestHeldMember(t, job)

		origBound, origTerm := groupDrainBound, terminateJobObjectFunc
		defer func() { groupDrainBound, terminateJobObjectFunc = origBound, origTerm }()

		groupDrainBound = 200 * time.Millisecond
		terminateJobObjectFunc = func(windows.Handle, uint32) error { return nil }

		// runJobDrain itself never calls scanSurvivorsFunc: the survivor
		// scan is drainCaptureJob's own step, exercised separately by
		// TestDrainCaptureJob_TeardownRecordAndSurvivorScan.
		result := runJobDrain(job)
		if result.DrainPolls <= 1 {
			t.Errorf("DrainPolls = %d, want > 1", result.DrainPolls)
		}
		if result.ActiveLast == 0 {
			t.Errorf("ActiveLast = 0, want > 0 (termination was faked to do nothing)")
		}
	})

	t.Run("real termination settles the job with one poll left active before it", func(t *testing.T) {
		job, cleanup := newCaptureTestJob(t)
		defer cleanup()
		startCaptureTestHeldMember(t, job)

		result := runJobDrain(job)
		if result.ActiveLast != 0 {
			t.Errorf("ActiveLast = %d, want 0 (the real termination settles the job)", result.ActiveLast)
		}
	})

	t.Run("failed termination is recorded but does not stop polling", func(t *testing.T) {
		job, cleanup := newCaptureTestJob(t)
		defer cleanup()
		startCaptureTestHeldMember(t, job)

		origBound, origTerm := groupDrainBound, terminateJobObjectFunc
		defer func() { groupDrainBound, terminateJobObjectFunc = origBound, origTerm }()
		wantErr := errors.New("injected termination failure")
		groupDrainBound = 200 * time.Millisecond
		terminateJobObjectFunc = func(windows.Handle, uint32) error { return wantErr }

		result := runJobDrain(job)
		if !errors.Is(result.DrainTerminateErr, wantErr) {
			t.Errorf("DrainTerminateErr = %v, want %v", result.DrainTerminateErr, wantErr)
		}
		if result.DrainPolls <= 1 {
			t.Errorf("DrainPolls = %d, want > 1 (a failed termination must not stop the loop)", result.DrainPolls)
		}
	})

	t.Run("a handle that is not a Job Object stops after one poll with a query error", func(t *testing.T) {
		notAJob := windows.CurrentProcess()

		result := runJobDrain(notAJob)
		if result.DrainPolls != 1 {
			t.Errorf("DrainPolls = %d, want 1 (a failed query must stop the loop)", result.DrainPolls)
		}
		if result.DrainQueryErr == nil {
			t.Error("DrainQueryErr = nil, want an error from querying a non-Job-Object handle")
		}
	})
}

func TestStartCapture_ResumeSeamFailure(t *testing.T) {
	spy := &captureWinLogSpy{}
	logger := slog.New(spy)

	origResume := resumeProcess
	t.Cleanup(func() { resumeProcess = origResume })
	wantErr := errors.New("injected resume failure")
	resumeProcess = func(int) error { return wantErr }

	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	cmd := exec.Command("cmd.exe", "/C", "echo x> "+markerPath) //nolint:gosec // fixed literal script

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, err := StartCapture(cmd, CaptureParams{Logger: logger})
		done <- outcome{err}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartCapture() did not return within 5s, want the failed resume to fail the start")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("StartCapture() took %v, want within 3s", elapsed)
	}

	var stageErr *StartError
	if !errors.As(oc.err, &stageErr) || stageErr.Stage != StageProcessResume {
		t.Fatalf("StartCapture() error = %v, want *StartError{Stage: StageProcessResume}", oc.err)
	}

	if _, statErr := os.Stat(markerPath); statErr == nil {
		t.Error("marker file exists, want the script never to have resumed")
	}
	if cmd.Process == nil {
		t.Fatal("cmd.Process = nil, want it set once cmd.Start succeeded")
	}
	assertCaptureWinProcessGone(t, cmd.Process.Pid, 3*time.Second)

	records := spy.snapshot()
	var sawResumeFailed, sawTeardown int
	for _, r := range records {
		if r.Msg == "process resume failed" {
			sawResumeFailed++
			if _, ok := r.Attrs["command"]; !ok {
				t.Error("process resume failed record missing command attribute")
			}
			if _, ok := r.Attrs["dir"]; !ok {
				t.Error("process resume failed record missing dir attribute")
			}
			if errAttr, ok := r.Attrs["error"]; !ok || errAttr.Any() == nil {
				t.Error("process resume failed record missing an error attribute")
			}
		}
		if r.Msg == "subprocess tree did not settle" || r.Msg == "subprocess tree settled" {
			sawTeardown++
		}
	}
	if sawResumeFailed != 1 {
		t.Errorf("process resume failed record count = %d, want 1", sawResumeFailed)
	}
	if sawTeardown != 1 {
		t.Errorf("teardown record count = %d, want 1", sawTeardown)
	}
}

func TestStartWithOwnedPipes_ResumeSeamFailure(t *testing.T) {
	spy := &captureWinLogSpy{}
	logger := slog.New(spy)

	origResume := resumeProcess
	t.Cleanup(func() { resumeProcess = origResume })
	wantErr := errors.New("injected resume failure")
	resumeProcess = func(int) error { return wantErr }

	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	cmd := exec.Command("cmd.exe", "/C", "echo x> "+markerPath) //nolint:gosec // fixed literal script

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, _, err := StartWithOwnedPipes(cmd, logger)
		done <- outcome{err}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartWithOwnedPipes() did not return within 5s, want the failed resume to fail the start")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("StartWithOwnedPipes() took %v, want within 3s", elapsed)
	}

	var stageErr *StartError
	if !errors.As(oc.err, &stageErr) || stageErr.Stage != StageProcessResume {
		t.Fatalf("StartWithOwnedPipes() error = %v, want *StartError{Stage: StageProcessResume}", oc.err)
	}
	if _, statErr := os.Stat(markerPath); statErr == nil {
		t.Error("marker file exists, want the script never to have resumed")
	}
	if cmd.Process == nil {
		t.Fatal("cmd.Process = nil, want it set once cmd.Start succeeded")
	}
	assertCaptureWinProcessGone(t, cmd.Process.Pid, 3*time.Second)

	var sawResumeFailed int
	for _, r := range spy.snapshot() {
		if r.Msg == "process resume failed" {
			sawResumeFailed++
			if errAttr, ok := r.Attrs["error"]; !ok || errAttr.Any() == nil {
				t.Error("process resume failed record missing an error attribute")
			}
		}
	}
	if sawResumeFailed != 1 {
		t.Errorf("process resume failed record count = %d, want 1", sawResumeFailed)
	}
}

func TestStartCapture_AssignJobObjectFailure(t *testing.T) {
	spy := &captureWinLogSpy{}
	logger := slog.New(spy)

	origAssign := assignToJobObjectFunc
	t.Cleanup(func() { assignToJobObjectFunc = origAssign })
	wantErr := errors.New("injected assignment failure")
	assignToJobObjectFunc = func(int, bool) (windows.Handle, windows.Handle, error) {
		return 0, 0, wantErr
	}

	var recordedJob windows.Handle
	origResumeSeam := resumeSeam
	t.Cleanup(func() { resumeSeam = origResumeSeam })

	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	cmd := exec.Command("cmd.exe", "/C", "echo x> "+markerPath) //nolint:gosec // fixed literal script
	resumeSeam = func() {
		if g := lookupGroup(cmd); g != nil {
			recordedJob = jobHandleOf(g)
		} else {
			recordedJob = windows.InvalidHandle
		}
	}

	type outcome struct {
		c   *Capture
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		c, err := StartCapture(cmd, CaptureParams{Logger: logger})
		done <- outcome{c, err}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartCapture() did not return within 5s")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("StartCapture() took %v, want within 3s", elapsed)
	}
	if oc.err != nil {
		t.Fatalf("StartCapture() error = %v, want nil (a failed assignment is fail-open)", oc.err)
	}
	if cmd.Process == nil {
		t.Fatal("cmd.Process = nil, want it set once cmd.Start succeeded")
	}

	if recordedJob != 0 {
		t.Errorf("registered job handle = %v, want 0 (zero job handle on a failed assignment)", recordedJob)
	}

	result := waitCaptureWinResult(t, oc.c, 10*time.Second)
	if result.WaitErr != nil {
		t.Errorf("Wait() WaitErr = %v, want nil (the process must still be resumed)", result.WaitErr)
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Errorf("marker file stat error = %v, want the process still resumed", statErr)
	}

	var sawAssignFailed, sawTeardown int
	for _, r := range spy.snapshot() {
		if r.Msg == "process group assignment failed" {
			sawAssignFailed++
			if _, ok := r.Attrs["command"]; !ok {
				t.Error("process group assignment failed record missing command attribute")
			}
			if _, ok := r.Attrs["dir"]; !ok {
				t.Error("process group assignment failed record missing dir attribute")
			}
			if errAttr, ok := r.Attrs["error"]; !ok || errAttr.Any() == nil {
				t.Error("process group assignment failed record missing an error attribute")
			}
		}
		if r.Msg == "subprocess tree did not settle" || r.Msg == "subprocess tree settled" {
			sawTeardown++
		}
	}
	if sawAssignFailed != 1 {
		t.Errorf("process group assignment failed record count = %d, want 1", sawAssignFailed)
	}
	if sawTeardown != 1 {
		t.Errorf("teardown record count = %d, want 1", sawTeardown)
	}
}

func TestStartWithOwnedPipes_AssignJobObjectFailure(t *testing.T) {
	spy := &captureWinLogSpy{}
	logger := slog.New(spy)

	origAssign := assignToJobObjectFunc
	t.Cleanup(func() { assignToJobObjectFunc = origAssign })
	wantErr := errors.New("injected assignment failure")
	assignToJobObjectFunc = func(int, bool) (windows.Handle, windows.Handle, error) {
		return 0, 0, wantErr
	}

	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	cmd := exec.Command("cmd.exe", "/C", "echo x> "+markerPath) //nolint:gosec // fixed literal script

	type outcome struct {
		pipes *OwnedPipes
		group *Group
		err   error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		pipes, group, err := StartWithOwnedPipes(cmd, logger)
		done <- outcome{pipes, group, err}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartWithOwnedPipes() did not return within 5s")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("StartWithOwnedPipes() took %v, want within 3s", elapsed)
	}
	if oc.err != nil {
		t.Fatalf("StartWithOwnedPipes() error = %v, want nil (a failed assignment is fail-open)", oc.err)
	}
	if oc.pipes != nil {
		defer oc.pipes.Close() //nolint:errcheck // best-effort
	}
	if cmd.Process == nil {
		t.Fatal("cmd.Process = nil, want it set once cmd.Start succeeded")
	}

	if got := jobHandleOf(oc.group); got != 0 {
		t.Errorf("registered job handle = %v, want 0 (zero job handle on a failed assignment)", got)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case waitErr := <-waitDone:
		if waitErr != nil {
			t.Errorf("cmd.Wait() error = %v, want nil (the process must still be resumed)", waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmd.Wait() did not return within 5s")
	}
	_ = oc.group.Kill()

	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Errorf("marker file stat error = %v, want the process still resumed", statErr)
	}

	var sawAssignFailed int
	for _, r := range spy.snapshot() {
		if r.Msg == "process group assignment failed" {
			sawAssignFailed++
			if _, ok := r.Attrs["command"]; !ok {
				t.Error("process group assignment failed record missing command attribute")
			}
			if _, ok := r.Attrs["dir"]; !ok {
				t.Error("process group assignment failed record missing dir attribute")
			}
			if errAttr, ok := r.Attrs["error"]; !ok || errAttr.Any() == nil {
				t.Error("process group assignment failed record missing an error attribute")
			}
		}
	}
	if sawAssignFailed != 1 {
		t.Errorf("process group assignment failed record count = %d, want 1", sawAssignFailed)
	}
}

func TestStartCapture_ResumeSeamCancelsRegisteredCapture(t *testing.T) {
	spy := &captureWinLogSpy{}
	logger := slog.New(spy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd.exe", "/C", "exit 0") //nolint:gosec // fixed literal script
	SetGroupKill(cmd)

	origResumeSeam := resumeSeam
	t.Cleanup(func() { resumeSeam = origResumeSeam })
	resumeSeam = func() {
		g := lookupGroup(cmd)
		job := jobHandleOf(g)
		cancel()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if running, err := jobHasRunningMember(job); err == nil && !running {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	before := len(spy.snapshot())
	c, err := StartCapture(cmd, CaptureParams{Logger: logger})
	if err != nil {
		var stageErr *StartError
		if !errors.As(err, &stageErr) || stageErr.Stage != StageProcessResume {
			t.Fatalf("StartCapture() error = %v, want nil or *StartError{Stage: StageProcessResume}", err)
		}
	} else {
		_ = waitCaptureWinResult(t, c, 5*time.Second)
	}

	record, ok := latestCaptureTeardownRecord(spy, before)
	if !ok {
		t.Fatalf("no teardown record captured")
	}
	if record.Msg != "subprocess tree settled" {
		t.Errorf("teardown record message = %q, want %q", record.Msg, "subprocess tree settled")
	}
	if polls, ok := record.Attrs["drain_polls"]; !ok || polls.Int64() < 1 {
		t.Errorf("drain_polls = %v (present=%v), want >= 1", polls, ok)
	}
	if _, ok := record.Attrs["drain_query_err"]; ok {
		t.Error("teardown record carries drain_query_err, want none")
	}
}

func newCaptureTestJob(t *testing.T) (windows.Handle, func()) {
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
	return job, func() { _ = windows.CloseHandle(job) }
}

func startCaptureTestHeldMember(t *testing.T, job windows.Handle) {
	t.Helper()
	newCaptureTestHeldMember(t, job)
}

func newCaptureTestHeldMember(t *testing.T, job windows.Handle) *exec.Cmd {
	t.Helper()
	path := agenttest.FakeRuntime(t, t.TempDir(), "member", agenttest.OutputScenario, agenttest.Output{Hang: true})
	cmd := exec.Command(path) //nolint:gosec // fake runtime path under t.TempDir()
	cmd.SysProcAttr = suspendedSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	processID, err := dwordPID(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("dwordPID() error = %v", err)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, processID)
	if err != nil {
		t.Fatalf("OpenProcess() error = %v", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		t.Fatalf("AssignProcessToJobObject() error = %v", err)
	}
	if err := ntResumeProcess(handle); err != nil {
		t.Fatalf("ntResumeProcess() error = %v", err)
	}
	return cmd
}

func TestDrainCaptureJob_TeardownRecordAndSurvivorScan(t *testing.T) {
	t.Run("unsettled job: the scan runs once and the record takes the Warn arm", func(t *testing.T) {
		job, _ := newCaptureTestJob(t)
		cmd := newCaptureTestHeldMember(t, job)

		origBound, origTerm, origScan := groupDrainBound, terminateJobObjectFunc, scanSurvivorsFunc
		defer func() { groupDrainBound, terminateJobObjectFunc, scanSurvivorsFunc = origBound, origTerm, origScan }()

		var scanCalls int
		groupDrainBound = 200 * time.Millisecond
		terminateJobObjectFunc = func(windows.Handle, uint32) error { return nil }
		scanSurvivorsFunc = func(uint32, []uint32) ([]jobSurvivor, error) {
			scanCalls++
			return nil, nil
		}

		spy := &captureWinLogSpy{}
		logger := slog.New(spy)

		// drainCaptureJob closes job; no further cleanup needed.
		drainCaptureJob(uintptr(job), cmd, time.Now(), 0, logger)

		if scanCalls != 1 {
			t.Errorf("scanSurvivorsFunc call count = %d, want 1 (the survivor scan must run when the drain leaves an active process)", scanCalls)
		}
		record, ok := latestCaptureTeardownRecord(spy, 0)
		if !ok {
			t.Fatalf("no teardown record captured")
		}
		if record.Msg != "subprocess tree did not settle" {
			t.Errorf("teardown record message = %q, want %q", record.Msg, "subprocess tree did not settle")
		}
	})

	t.Run("settled job: the scan does not run and the record takes the Debug arm", func(t *testing.T) {
		job, _ := newCaptureTestJob(t)
		cmd := newCaptureTestHeldMember(t, job)

		origScan := scanSurvivorsFunc
		defer func() { scanSurvivorsFunc = origScan }()
		var scanCalls int
		scanSurvivorsFunc = func(uint32, []uint32) ([]jobSurvivor, error) {
			scanCalls++
			return nil, nil
		}

		spy := &captureWinLogSpy{}
		logger := slog.New(spy)

		// The real termination settles the job well within groupDrainBound.
		drainCaptureJob(uintptr(job), cmd, time.Now(), 0, logger)

		if scanCalls != 0 {
			t.Errorf("scanSurvivorsFunc call count = %d, want 0 (the survivor scan must not run once the job has settled)", scanCalls)
		}
		record, ok := latestCaptureTeardownRecord(spy, 0)
		if !ok {
			t.Fatalf("no teardown record captured")
		}
		if record.Msg != "subprocess tree settled" {
			t.Errorf("teardown record message = %q, want %q", record.Msg, "subprocess tree settled")
		}
	})
}

func TestProcessIsRunning(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "ping -n 30 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a Windows PID fits in uint32

	if !processIsRunning(pid) {
		t.Error("processIsRunning(running process) = false, want true")
	}

	// Hold a handle of our own so the process object, and therefore the
	// identifier, survives the exit. That is the state in which opening
	// the PID succeeds for a process that is already gone.
	pinned, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if openErr != nil {
		t.Fatalf("OpenProcess() = %v, want nil", openErr)
	}
	defer func() { _ = windows.CloseHandle(pinned) }()

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	if processIsRunning(pid) {
		t.Error("processIsRunning(exited process whose handle is still held) = true, want false")
	}
}
