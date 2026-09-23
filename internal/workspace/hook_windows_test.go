//go:build windows

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
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

// windowsHookLogRecord is one record captured by [windowsHookLogSpy].
type windowsHookLogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

// windowsHookLogSpy is a slog.Handler that captures every log record's
// level, message, and attribute set, so a test can assert on the
// record procutil emits through slog.Default while draining RunHook's
// capture: RunHook itself passes no Logger, so procutil's teardown and
// abandonment records land on slog.Default.
type windowsHookLogSpy struct {
	mu      sync.Mutex
	records []windowsHookLogRecord
}

func (s *windowsHookLogSpy) Enabled(context.Context, slog.Level) bool { return true }

func (s *windowsHookLogSpy) Handle(_ context.Context, r slog.Record) error {
	rec := windowsHookLogRecord{Level: r.Level, Msg: r.Message, Attrs: make(map[string]slog.Value, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.mu.Unlock()
	return nil
}

func (s *windowsHookLogSpy) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *windowsHookLogSpy) WithGroup(_ string) slog.Handler      { return s }

func (s *windowsHookLogSpy) snapshot() []windowsHookLogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]windowsHookLogRecord, len(s.records))
	copy(out, s.records)
	return out
}

// installWindowsHookLogSpy replaces slog.Default with a spy logger for
// the duration of the test and restores the previous default through
// t.Cleanup. The caller MUST be a non-parallel top-level test:
// slog.SetDefault is process-global.
func installWindowsHookLogSpy(t *testing.T) *windowsHookLogSpy {
	t.Helper()
	spy := &windowsHookLogSpy{}
	orig := slog.Default()
	slog.SetDefault(slog.New(spy))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return spy
}

// latestWindowsHookTeardownRecord returns the most recent teardown
// record (either message [procutil]'s job-drain logs) captured at
// index from or later whose dir attribute is dir.
func latestWindowsHookTeardownRecord(spy *windowsHookLogSpy, from int, dir string) (windowsHookLogRecord, bool) {
	records := spy.snapshot()
	for i := len(records) - 1; i >= from; i-- {
		if records[i].Msg != "subprocess tree did not settle" && records[i].Msg != "subprocess tree settled" {
			continue
		}
		if recordDir, ok := records[i].Attrs["dir"]; ok && recordDir.String() == dir {
			return records[i], true
		}
	}
	return windowsHookLogRecord{}, false
}

// TestRunHook_HeldBackgroundProcessSucceedsWithSettledTeardown pins
// that a script that echoes a line and starts a background process
// holding the output, then exits 0, makes RunHook return within 3s with
// the line and no error, logs no CaptureAbandonedWarning record, and
// the teardown record takes the Debug arm.
func TestRunHook_HeldBackgroundProcessSucceedsWithSettledTeardown(t *testing.T) {
	spy := installWindowsHookLogSpy(t)

	dir := t.TempDir()
	before := len(spy.snapshot())

	start := time.Now()
	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo hello & start /b ping -n 30 127.0.0.1",
		Dir:       dir,
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("RunHook() took %v, want within 3s", elapsed)
	}
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "hello")
	}

	for _, r := range spy.snapshot()[before:] {
		if r.Msg == procutil.CaptureAbandonedWarning {
			t.Errorf("log contains %q, want no abandonment WARN", procutil.CaptureAbandonedWarning)
		}
	}

	record, ok := latestWindowsHookTeardownRecord(spy, before, dir)
	if !ok {
		t.Fatalf("no teardown record captured")
	}
	// The Debug arm requires an empty Survivors slice among its other
	// conditions, so checking it here on top of the message would be
	// vacuous: it cannot distinguish a scan that ran and found nothing
	// from a scan that was skipped entirely. Whether the scan itself
	// runs only when the job is left unsettled is proven directly in
	// procutil, the package that owns the scanSurvivorsFunc seam this
	// package has no access to.
	if record.Msg != "subprocess tree settled" {
		t.Errorf("teardown record message = %q, want %q", record.Msg, "subprocess tree settled")
	}
	if activeLast, ok := record.Attrs["active_last"]; !ok || activeLast.Int64() != 0 {
		t.Errorf("teardown record active_last = %v (present=%v), want 0", activeLast, ok)
	}
}

// runHookHelperParams configures the workspace.run-hook-helper
// scenario TestRunHook_DetachedHelperReportsTerminatedLeftovers drives
// from a process with no console of its own.
type runHookHelperParams struct {
	Script     string
	Dir        string
	TimeoutMS  int
	ResultPath string
}

// runHookHelperResult is the JSON document runHookHelperScenario
// writes to ResultPath. RunHookMS is the wall-clock duration of the
// helper's own RunHook call, letting the test measure the bound RunHook
// itself promises instead of the round trip through the detached
// process's own startup.
type runHookHelperResult struct {
	Err                 string
	TerminatedLeftovers bool
	RunHookMS           int64
}

func runHookHelperScenario(_ []string, p runHookHelperParams) int {
	start := time.Now()
	result, err := RunHook(context.Background(), HookParams{
		Script:    p.Script,
		Dir:       p.Dir,
		Env:       map[string]string{},
		TimeoutMS: p.TimeoutMS,
	})
	out := runHookHelperResult{TerminatedLeftovers: result.TerminatedLeftovers, RunHookMS: time.Since(start).Milliseconds()}
	if err != nil {
		out.Err = err.Error()
	}
	data, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return 2
	}
	// The reader polls for the file and retries only a failed read, so a
	// path it can open while the write is still in flight hands it a
	// partial document it cannot parse. Renaming a fully written file
	// into place leaves nothing at ResultPath for it to read early.
	tmpPath := p.ResultPath + ".tmp"
	if writeErr := os.WriteFile(tmpPath, data, 0o600); writeErr != nil {
		return 2
	}
	if renameErr := os.Rename(tmpPath, p.ResultPath); renameErr != nil {
		return 2
	}
	return 0
}

func init() {
	fakeScenarios["workspace.run-hook-helper"] = agenttest.Typed(runHookHelperScenario)
}

// TestRunHook_DetachedHelperReportsTerminatedLeftovers pins that, run
// from a helper process with no console of its own (DETACHED_PROCESS),
// a script that leaves a background process running makes RunHook
// report TerminatedLeftovers true; a script that exits cleanly reports
// it false. The helper answers within 20s or the test fails.
func TestRunHook_DetachedHelperReportsTerminatedLeftovers(t *testing.T) {
	tests := []struct {
		name         string
		script       string
		wantLeftover bool
	}{
		{"background process left running", "start /b ping -n 30 127.0.0.1 >NUL", true},
		{"clean exit", "exit 0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runDir := t.TempDir()
			resultPath := filepath.Join(t.TempDir(), "result.json")
			helperPath := agenttest.FakeRuntime(t, t.TempDir(), "helper", "workspace.run-hook-helper", runHookHelperParams{
				Script:     tt.script,
				Dir:        runDir,
				TimeoutMS:  5000,
				ResultPath: resultPath,
			})

			cmd := exec.Command(helperPath) //nolint:gosec // fake runtime path under t.TempDir()
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			})

			deadline := time.Now().Add(20 * time.Second)
			var data []byte
			for time.Now().Before(deadline) {
				if b, readErr := os.ReadFile(resultPath); readErr == nil {
					data = b
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if data == nil {
				t.Fatal("no answer from the detached helper within 20s")
			}

			var out runHookHelperResult
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("unmarshal helper result: %v", err)
			}
			if out.Err != "" {
				t.Fatalf("helper's RunHook() error = %s, want nil", out.Err)
			}
			if out.TerminatedLeftovers != tt.wantLeftover {
				t.Errorf("TerminatedLeftovers = %v, want %v", out.TerminatedLeftovers, tt.wantLeftover)
			}
			if out.RunHookMS > 3000 {
				t.Errorf("helper's RunHook() took %dms, want within 3000ms", out.RunHookMS)
			}
		})
	}
}
