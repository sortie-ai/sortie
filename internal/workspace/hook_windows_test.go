//go:build windows

package workspace

import (
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
)

func requireHookError(t *testing.T, err error) *HookError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *HookError, got nil")
	}
	var he *HookError
	if !errors.As(err, &he) {
		t.Fatalf("error type = %T, want *HookError", err)
	}
	return he
}

func TestRunHook_HappyPath(t *testing.T) {
	t.Parallel()

	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo hello",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "hello")
	}
}

func TestRunHook_EnvVars(t *testing.T) {
	t.Parallel()

	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo %SORTIE_FOO%",
		Dir:       t.TempDir(),
		Env:       map[string]string{"SORTIE_FOO": "bar"},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}
	if !strings.Contains(result.Output, "bar") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "bar")
	}
}

func TestRunHook_Cwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// EvalSymlinks resolves any symlinks in the temp path.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}

	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo %CD%",
		Dir:       dir,
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}

	got := strings.TrimSpace(result.Output)
	// %CD% may return an 8.3 short name (e.g. RUNNER~1) on Windows CI.
	// EvalSymlinks expands short names to long names via GetFinalPathNameByHandle.
	if resolved, err2 := filepath.EvalSymlinks(got); err2 == nil {
		got = resolved
	}
	if !strings.EqualFold(got, realDir) {
		t.Errorf("%%CD%% = %q, want %q", got, realDir)
	}
}

func TestRunHook_NonZeroExit(t *testing.T) {
	t.Parallel()

	_, err := RunHook(context.Background(), HookParams{
		Script:    "exit 1",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})

	he := requireHookError(t, err)
	if he.Op != "run" {
		t.Errorf("HookError.Op = %q, want %q", he.Op, "run")
	}
	if he.ExitCode != 1 {
		t.Errorf("HookError.ExitCode = %d, want 1", he.ExitCode)
	}
}

func TestRunHook_Timeout(t *testing.T) {
	t.Parallel()

	// "ping -n 30" blocks for ~29 s via the network timer, not stdin, so it
	// reliably times out even when stdin is /dev/null (the Go exec default).
	_, err := RunHook(context.Background(), HookParams{
		Script:    "ping -n 30 127.0.0.1",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 200,
	})

	he := requireHookError(t, err)
	if he.Op != "timeout" {
		t.Errorf("HookError.Op = %q, want %q", he.Op, "timeout")
	}
	if he.ExitCode != -1 {
		t.Errorf("HookError.ExitCode = %d, want -1", he.ExitCode)
	}
}

// hookProcessTreeKillScript spawns a background child and blocks the
// parent, both with ping (not pause): pause exits immediately when
// stdin is /dev/null, but ping blocks on the network timer regardless
// of stdin. Both processes must be killed by the Job Object when the
// timeout fires.
const hookProcessTreeKillScript = "start /b ping.exe -n 30 127.0.0.1 & ping -n 30 127.0.0.1"

// hookNoBackgroundChildScript is hookProcessTreeKillScript with the
// background child removed, so there is nothing for the job to fail
// to catch.
const hookNoBackgroundChildScript = "ping -n 30 127.0.0.1"

// TestWindowsSuspendedProcessResumesViaThreadSnapshot confirms, on a
// Windows runner and independently of any production helper, the
// platform assumption the rest of this file's suspend-then-assign
// fix depends on: that a system-wide TH32CS_SNAPTHREAD snapshot
// enumerates the primary thread of a process created with
// CREATE_SUSPENDED, and that resuming it starts the process. It
// duplicates the minimal suspend/enumerate/resume sequence rather
// than calling resumeHookProcess, because it exists to validate the
// platform's behavior, not the helper's correctness.
func TestWindowsSuspendedProcessResumesViaThreadSnapshot(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "exit 0") //nolint:gosec // G204: fixed literal script, not user input
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a Windows PID never exceeds uint32 range

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		t.Fatalf("CreateToolhelp32Snapshot() error: %v", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		t.Fatalf("Thread32First() error: %v", err)
	}

	var primaryTID uint32
	for {
		if entry.OwnerProcessID == pid {
			primaryTID = entry.ThreadID
			break
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			break
		}
	}
	if primaryTID == 0 {
		t.Fatalf("no thread found owned by pid %d", pid)
	}

	handle, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, primaryTID)
	if err != nil {
		t.Fatalf("OpenThread() error: %v", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	const maxAttempts = 8
	resolved := false
	for range maxAttempts {
		prev, resumeErr := windows.ResumeThread(handle)
		if resumeErr != nil {
			t.Fatalf("ResumeThread() error: %v", resumeErr)
		}
		if prev == 1 {
			resolved = true
			break
		}
	}
	if !resolved {
		t.Fatalf("ResumeThread() previous suspend count never reached 1 within %d attempts", maxAttempts)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		if waitErr != nil {
			t.Errorf("Wait() error: %v, want the resumed process to exit 0", waitErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process did not complete within 10s of resume")
	}
}

// windowsLogRecord is one record captured by [windowsLogSpy].
type windowsLogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

// windowsLogSpy is a [slog.Handler] that captures every log record's
// level, message, and attribute set, so a test can assert on the
// record RunHook emits. It is local to this package rather than
// reusing internal/agent/agenttest's equivalent, because importing
// that package would create a workspace -> agent import edge; the
// technique, not the code, is shared.
type windowsLogSpy struct {
	mu      sync.Mutex
	records []windowsLogRecord
}

func (s *windowsLogSpy) Enabled(context.Context, slog.Level) bool { return true }

func (s *windowsLogSpy) Handle(_ context.Context, r slog.Record) error {
	rec := windowsLogRecord{Level: r.Level, Msg: r.Message, Attrs: make(map[string]slog.Value, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.mu.Unlock()
	return nil
}

func (s *windowsLogSpy) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *windowsLogSpy) WithGroup(_ string) slog.Handler      { return s }

// snapshot returns a copy of every record captured so far.
func (s *windowsLogSpy) snapshot() []windowsLogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]windowsLogRecord, len(s.records))
	copy(out, s.records)
	return out
}

// installWindowsLogSpy replaces [slog.Default] with a spy logger for
// the duration of the test and restores the previous default through
// [testing.T.Cleanup]. The caller MUST be a non-parallel top-level
// test: slog.SetDefault is process-global, and Go's testing package
// releases paused parallel tests only after every non-parallel
// top-level test has already run to completion, so a non-parallel
// top-level test cannot overlap a parallel sibling.
func installWindowsLogSpy(t *testing.T) *windowsLogSpy {
	t.Helper()
	spy := &windowsLogSpy{}
	orig := slog.Default()
	slog.SetDefault(slog.New(spy))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return spy
}

// latestTeardownRecord returns the most recent hook-teardown record
// (either message [logHookTeardown] emits) captured at index from or
// later, so a caller can isolate the record produced by one RunHook
// invocation from records any earlier invocation left behind.
func latestTeardownRecord(spy *windowsLogSpy, from int) (windowsLogRecord, bool) {
	records := spy.snapshot()
	for i := len(records) - 1; i >= from; i-- {
		if records[i].Msg == "hook process tree did not settle" || records[i].Msg == "hook process tree settled" {
			return records[i], true
		}
	}
	return windowsLogRecord{}, false
}

// TestRunHook_TeardownRecordShape asserts, in process, that RunHook
// emits the mandatory teardown record shape on a passing test run
// rather than relying on a human to read it out of a log.
func TestRunHook_TeardownRecordShape(t *testing.T) {
	spy := installWindowsLogSpy(t)

	before := len(spy.snapshot())
	_, err := RunHook(context.Background(), HookParams{
		Script:    hookProcessTreeKillScript,
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 300,
	})
	_ = requireHookError(t, err)

	record, ok := latestTeardownRecord(spy, before)
	if !ok {
		t.Fatalf("no teardown record captured")
	}

	if _, ok := record.Attrs["total_processes"]; !ok {
		t.Errorf("record missing attribute %q", "total_processes")
	}
	if _, ok := record.Attrs["root_openable"]; !ok {
		t.Errorf("record missing attribute %q", "root_openable")
	}
	if _, ok := record.Attrs["root_in_job_list"]; !ok {
		t.Errorf("record missing attribute %q", "root_in_job_list")
	}
}

// runHookScenarioTotalProcesses runs the process-tree-kill scenario
// once and returns the total_processes attribute from its captured
// teardown record.
func runHookScenarioTotalProcesses(t *testing.T, spy *windowsLogSpy) int64 {
	t.Helper()
	before := len(spy.snapshot())
	_, err := RunHook(context.Background(), HookParams{
		Script:    hookProcessTreeKillScript,
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 300,
	})
	_ = requireHookError(t, err)

	record, ok := latestTeardownRecord(spy, before)
	if !ok {
		t.Fatalf("no teardown record captured for this invocation")
	}
	total, ok := record.Attrs["total_processes"]
	if !ok {
		t.Fatalf("teardown record missing total_processes")
	}
	return total.Int64()
}

// TestRunHook_EscapeWithSeamDelay reproduces the start-to-assign
// escape on demand through the hookAssignDelay seam, and asserts the
// paired comparison the seam exists to make possible: a delay before
// job assignment cannot lower the observed process count once the
// process is created suspended. No process count is hard-coded.
func TestRunHook_EscapeWithSeamDelay(t *testing.T) {
	spy := installWindowsLogSpy(t)

	hookAssignDelay.Store(0)
	t.Cleanup(func() { hookAssignDelay.Store(0) })

	var clean int64
	for range 3 {
		if v := runHookScenarioTotalProcesses(t, spy); v > clean {
			clean = v
		}
	}

	hookAssignDelay.Store(int64(50 * time.Millisecond))
	delayed := runHookScenarioTotalProcesses(t, spy)

	if delayed < clean {
		t.Errorf("total_processes with the seam delay = %d, want >= the clean reference %d", delayed, clean)
	}
}

// hookProbeClass is the outcome of re-probing a directory's
// removability on a fixed cadence and window, after an assertion that
// depends on the outcome has already been recorded.
type hookProbeClass string

const (
	hookProbeTeardownLag hookProbeClass = "teardown_lag"
	hookProbeAmbiguous   hookProbeClass = "ambiguous"
	hookProbeLiveHolder  hookProbeClass = "live_holder"
)

// probeDirRemovable re-probes dir's removability at a 25 ms cadence
// for up to a 5 s window, without changing any verdict the caller has
// already recorded, and classifies how long the release took. The
// window sits well above any plausible handle-teardown lag and well
// below the roughly 29 s lifetime of the ping process the scenarios
// in this file spawn, so the two classes cannot be confused with a
// live holder.
func probeDirRemovable(dir string) hookProbeClass {
	const (
		cadence = 25 * time.Millisecond
		window  = 5 * time.Second
	)
	start := time.Now()
	deadline := start.Add(window)
	for {
		if err := os.RemoveAll(dir); err == nil {
			if time.Since(start) <= 100*time.Millisecond {
				return hookProbeTeardownLag
			}
			return hookProbeAmbiguous
		}
		if time.Now().After(deadline) {
			return hookProbeLiveHolder
		}
		time.Sleep(cadence)
	}
}

// hookVerdicts reports which of the three independent conclusions the
// diagnosis can reach are true for one run. Any subset may be true
// together: an escape and a root lingering can both occur in the same
// run, which is why these are independent booleans rather than one
// classification.
type hookVerdicts struct {
	Escape        bool // V1: an escaped or short-counted descendant
	RootLingering bool // V2: the hook root outlived its removal from the job
	Environmental bool // V3: the control directory was also held
}

// evaluateHookVerdicts computes the three independent verdicts from
// one background-child invocation's captured teardown record, the
// clean total_processes reference, whether the hook directory was
// still held when checked, the no-background-child invocation's
// captured record and whether its directory was held, and whether the
// control directory was held.
func evaluateHookVerdicts(
	backgroundRecord windowsLogRecord,
	cleanReference int64,
	hookHeld bool,
	noChildRecord windowsLogRecord,
	noChildHeld bool,
	controlHeld bool,
) hookVerdicts {
	var v hookVerdicts

	if total, ok := backgroundRecord.Attrs["total_processes"]; ok && total.Int64() < cleanReference {
		v.Escape = true
	}
	if survivors, ok := backgroundRecord.Attrs["survivors"]; ok {
		if list, ok := survivors.Any().([]hookSurvivor); ok && len(list) > 0 {
			v.Escape = true
		}
	}
	_ = hookHeld // V1 rests on the count and the survivor list, not on the hold itself.

	if noChildHeld {
		if openable, ok := noChildRecord.Attrs["root_openable"]; ok && openable.Bool() {
			v.RootLingering = true
		}
	}

	v.Environmental = controlHeld

	return v
}

// TestRunHook_ProcessTreeKill exercises the background-child timeout
// scenario, asserts the existing prompt-return and immediate-removal
// invariants, and separates the four candidate causes of a held
// directory into three independent, positively observed verdicts
// rather than a single classification.
func TestRunHook_ProcessTreeKill(t *testing.T) {
	spy := installWindowsLogSpy(t)

	dir := t.TempDir()
	controlDir := t.TempDir()
	noChildDir := t.TempDir()

	start := time.Now()
	before := len(spy.snapshot())
	_, err := RunHook(context.Background(), HookParams{
		Script:    hookProcessTreeKillScript,
		Dir:       dir,
		Env:       map[string]string{},
		TimeoutMS: 300,
	})

	he := requireHookError(t, err)
	if he.Op != "timeout" {
		t.Errorf("HookError.Op = %q, want %q", he.Op, "timeout")
	}

	// RunHook must return promptly after the timeout — allow generous
	// slack for WaitDelay (3 s) plus scheduling jitter.
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("RunHook took %v after timeout; expected prompt return (< 5s)", elapsed)
	}

	backgroundRecord, ok := latestTeardownRecord(spy, before)
	if !ok {
		t.Fatalf("no teardown record captured for the background-child scenario")
	}

	// A future failing run's log carrying this marker and no teardown
	// record means the teardown path did not run; a log carrying
	// neither means the log is not carrying records at all, and no
	// inference from silence is valid without this marker.
	slog.Warn("hook process tree kill test asserting removal")

	// The drain guarantee: when RunHook returns, the job reports zero
	// active processes. Removal must succeed on the first immediate
	// attempt when nothing else is holding the directory. Before the
	// start-to-assign fix this failed intermittently with a sharing
	// violation raised by an escaped background child.
	immediateErr := os.RemoveAll(dir)
	if immediateErr != nil {
		t.Errorf("hook dir removal immediately after RunHook = %v, want success", immediateErr)
	}

	// Re-probe removability without changing the verdict recorded
	// above: it has already failed (or passed) and stays that way.
	hookHeld := immediateErr != nil
	hookClass := hookProbeTeardownLag
	if hookHeld {
		hookClass = probeDirRemovable(dir)
	}

	controlHeld := false
	controlClass := hookProbeTeardownLag
	if err := os.RemoveAll(controlDir); err != nil {
		controlHeld = true
		controlClass = probeDirRemovable(controlDir)
	}

	// A second scenario, identical except it spawns no background
	// child: there is nothing to escape the job, so a hold observed
	// here cannot be an escape.
	beforeNoChild := len(spy.snapshot())
	_, noChildErr := RunHook(context.Background(), HookParams{
		Script:    hookNoBackgroundChildScript,
		Dir:       noChildDir,
		Env:       map[string]string{},
		TimeoutMS: 300,
	})
	_ = requireHookError(t, noChildErr)
	noChildRecord, ok := latestTeardownRecord(spy, beforeNoChild)
	if !ok {
		t.Fatalf("no teardown record captured for the no-background-child scenario")
	}
	noChildHeld := false
	noChildClass := hookProbeTeardownLag
	if err := os.RemoveAll(noChildDir); err != nil {
		noChildHeld = true
		noChildClass = probeDirRemovable(noChildDir)
	}

	// Clean reference: the maximum total_processes observed across
	// three invocations of the background-child scenario with the
	// fault-injection seam at its zero, production value.
	var cleanReference int64
	for range 3 {
		if v := runHookScenarioTotalProcesses(t, spy); v > cleanReference {
			cleanReference = v
		}
	}

	verdicts := evaluateHookVerdicts(backgroundRecord, cleanReference, hookHeld, noChildRecord, noChildHeld, controlHeld)

	t.Logf(
		"verdicts: escape=%v root_lingering=%v environmental=%v; hook_held=%v hook_class=%s; "+
			"control_held=%v control_class=%s; no_child_held=%v no_child_class=%s; clean_reference=%d",
		verdicts.Escape, verdicts.RootLingering, verdicts.Environmental,
		hookHeld, hookClass, controlHeld, controlClass, noChildHeld, noChildClass, cleanReference,
	)

	switch {
	case !hookHeld && !verdicts.Escape && !verdicts.RootLingering && !verdicts.Environmental:
		t.Logf("summary: clean (nothing held, no verdict fired)")
	case hookHeld && !verdicts.Escape && !verdicts.RootLingering && !verdicts.Environmental:
		t.Logf("summary: unexplained (directory held, no verdict fired)")
	}
}

// hookStressIteration is one iteration's outcome inside
// [TestRunHook_Stress].
type hookStressIteration struct {
	immediateRemovalFailed bool
	waitDelayExpired       bool
	totalProcesses         int64
	survivorCount          int
	rootLingering          bool
	environmental          bool
	// holdClasses is the hold-duration class from every
	// probeDirRemovable call this iteration made (the hook directory,
	// the control directory, or both), for the run-wide histogram.
	holdClasses []hookProbeClass
}

// runHookStressIteration runs the background-child scenario once,
// classifies it against a paired control directory, and reports what
// was observed. t.TempDir handles cleanup even when the directory was
// never released.
func runHookStressIteration(t *testing.T, spy *windowsLogSpy) hookStressIteration {
	t.Helper()

	dir := t.TempDir()
	controlDir := t.TempDir()

	before := len(spy.snapshot())
	_, _ = RunHook(context.Background(), HookParams{
		Script:    hookProcessTreeKillScript,
		Dir:       dir,
		Env:       map[string]string{},
		TimeoutMS: 300,
	})
	record, _ := latestTeardownRecord(spy, before)

	var it hookStressIteration
	if total, ok := record.Attrs["total_processes"]; ok {
		it.totalProcesses = total.Int64()
	}
	if expired, ok := record.Attrs["wait_delay_expired"]; ok {
		it.waitDelayExpired = expired.Bool()
	}
	if survivors, ok := record.Attrs["survivors"]; ok {
		if list, ok := survivors.Any().([]hookSurvivor); ok {
			it.survivorCount = len(list)
		}
	}

	it.immediateRemovalFailed = os.RemoveAll(dir) != nil
	if it.immediateRemovalFailed {
		if openable, ok := record.Attrs["root_openable"]; ok && openable.Bool() {
			it.rootLingering = true
		}
		it.holdClasses = append(it.holdClasses, probeDirRemovable(dir))
	}

	if err := os.RemoveAll(controlDir); err != nil {
		it.environmental = true
		it.holdClasses = append(it.holdClasses, probeDirRemovable(controlDir))
	}

	return it
}

// hookStressReport summarizes one half (serial or concurrent) of a
// stress run for TestRunHook_Stress's report.
func hookStressReport(label string, iterations []hookStressIteration, cleanReference int64) string {
	var removalFailures, shortCount, survivorCount, rootLingering, environmental, waitDelayExpired int
	var teardownLag, ambiguous, liveHolder int
	for _, it := range iterations {
		if it.immediateRemovalFailed {
			removalFailures++
		}
		if it.totalProcesses < cleanReference {
			shortCount++
		}
		if it.survivorCount > 0 {
			survivorCount++
		}
		if it.rootLingering {
			rootLingering++
		}
		if it.environmental {
			environmental++
		}
		if it.waitDelayExpired {
			waitDelayExpired++
		}
		for _, class := range it.holdClasses {
			switch class {
			case hookProbeTeardownLag:
				teardownLag++
			case hookProbeAmbiguous:
				ambiguous++
			case hookProbeLiveHolder:
				liveHolder++
			}
		}
	}
	return fmt.Sprintf(
		"%s: attempted=%d immediate_removal_failed=%d v1_short_count=%d v1_survivor_count=%d "+
			"v2_root_lingering=%d v3_environmental=%d wait_delay_expired=%d "+
			"hold_class_teardown_lag=%d hold_class_ambiguous=%d hold_class_live_holder=%d",
		label, len(iterations), removalFailures, shortCount, survivorCount, rootLingering, environmental, waitDelayExpired,
		teardownLag, ambiguous, liveHolder,
	)
}

// TestRunHook_Stress runs the process-tree-kill scenario repeatedly,
// with a serial half and a concurrently loaded half, and reports the
// distribution of outcomes rather than a single pass/fail. It is
// gated because it is not part of the ordinary pull-request signal:
// its purpose is to accumulate a residual failure rate across many
// dispatches, not to catch a regression on every push.
func TestRunHook_Stress(t *testing.T) {
	if os.Getenv("SORTIE_HOOKSTRESS_TEST") != "1" {
		t.Skip("skipping hook stress test: set SORTIE_HOOKSTRESS_TEST=1 to enable")
	}

	iterations := 50
	if v := os.Getenv("SORTIE_HOOKSTRESS_ITERATIONS"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 {
			iterations = n
		}
	}

	spy := installWindowsLogSpy(t)
	overallDeadline := time.Now().Add(20 * time.Minute)

	var cleanReference int64
	var serial, concurrent []hookStressIteration

	for i := 0; i < iterations; i++ {
		if time.Now().After(overallDeadline) {
			t.Fatalf("hook stress test exceeded its 20-minute hang guard after %d of %d iterations", i, iterations)
		}

		if i < iterations/2 {
			it := runHookStressIteration(t, spy)
			if i < 5 && it.totalProcesses > cleanReference {
				cleanReference = it.totalProcesses
			}
			serial = append(serial, it)
			continue
		}

		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				loadDir := t.TempDir()
				_, _ = RunHook(context.Background(), HookParams{
					Script:    hookProcessTreeKillScript,
					Dir:       loadDir,
					Env:       map[string]string{},
					TimeoutMS: 300,
				})
				_ = os.RemoveAll(loadDir)
			})
		}
		it := runHookStressIteration(t, spy)
		wg.Wait()
		concurrent = append(concurrent, it)
	}

	summary := fmt.Sprintf("clean_reference=%d\n%s\n%s",
		cleanReference,
		hookStressReport("serial", serial, cleanReference),
		hookStressReport("concurrent", concurrent, cleanReference),
	)
	t.Log(summary)

	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		f, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr == nil {
			_, _ = f.WriteString(summary + "\n")
			_ = f.Close()
		}
	}
}
