package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
)

// validWorkflow returns a minimal valid WORKFLOW.md content with the
// given polling interval.
func validWorkflow(intervalMS int) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: %d\n---\nDo the task for {{ .issue.title }}.\n", intervalMS)
}

// retentionWorkflow returns a minimal WORKFLOW.md content with the given
// workspace.retention_days value.
func retentionWorkflow(days int) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: 5000\nworkspace:\n  retention_days: %d\n---\nDo the task for {{ .issue.title }}.\n", days)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeWorkflow overwrites path atomically via write-to-temp + rename,
// matching the pattern used by many editors. On Windows the rename can
// fail with "Access is denied" when fsnotify holds a handle on the
// target; the helper retries a few times with a brief back-off.
func writeWorkflow(t *testing.T, path string, content []byte) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".workflow.tmp")
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	var err error
	for range 5 {
		err = os.Rename(tmp, path)
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rename temp to target: %v", err)
}

// pollUntil calls fn repeatedly until it returns true or a 3-second
// deadline elapses. Returns whether the condition was met.
func pollUntil(fn func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

func TestManager_FilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
	}{
		{name: "standard name", filename: "WORKFLOW.md"},
		{name: "custom prefix", filename: "backend.WORKFLOW.md"},
		{name: "all lowercase", filename: "workflow.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, tt.filename)
			if err := os.WriteFile(path, validWorkflow(5000), 0o644); err != nil {
				t.Fatalf("write workflow file: %v", err)
			}
			m, err := NewManager(path, testLogger())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			m.Stop()

			if got := m.FilePath(); got != tt.filename {
				t.Errorf("FilePath() = %q, want %q", got, tt.filename)
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	t.Parallel()

	t.Run("ValidFile", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, validWorkflow(5000))

		mgr, err := NewManager(path, testLogger())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if mgr.Config().Polling.IntervalMS != 5000 {
			t.Errorf("Polling.IntervalMS = %d, want 5000", mgr.Config().Polling.IntervalMS)
		}
		if mgr.PromptTemplate() == nil {
			t.Error("PromptTemplate() is nil, want non-nil")
		}
		if mgr.LastLoadError() != nil {
			t.Errorf("LastLoadError() = %v, want nil", mgr.LastLoadError())
		}
	})

	t.Run("MissingFile", func(t *testing.T) {
		t.Parallel()

		_, err := NewManager(filepath.Join(t.TempDir(), "nonexistent.md"), testLogger())
		if err == nil {
			t.Fatal("NewManager() error = nil, want error")
		}
	})

	t.Run("InvalidYAML", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, []byte("---\n: : : bad {{{\n---\nprompt\n"))

		_, err := NewManager(path, testLogger())
		if err == nil {
			t.Fatal("NewManager() error = nil, want error")
		}
	})

	t.Run("InvalidTemplate", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, []byte("---\nk: v\n---\n{{ .issue.title\n"))

		_, err := NewManager(path, testLogger())
		if err == nil {
			t.Fatal("NewManager() error = nil, want error")
		}
	})
}

func TestManager_Reload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with new interval.
	mustWriteFile(t, path, validWorkflow(9999))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if mgr.Config().Polling.IntervalMS != 9999 {
		t.Errorf("after Reload: Polling.IntervalMS = %d, want 9999", mgr.Config().Polling.IntervalMS)
	}
	if mgr.LastLoadError() != nil {
		t.Errorf("after successful Reload: LastLoadError = %v, want nil", mgr.LastLoadError())
	}
}

func TestManager_ReloadRetainsOnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with invalid content.
	mustWriteFile(t, path, []byte("---\n[[[invalid\n---\nprompt\n"))

	err = mgr.Reload()
	if err == nil {
		t.Fatal("Reload() error = nil, want error")
	}
	if mgr.Config().Polling.IntervalMS != 5000 {
		t.Errorf("after failed Reload: Polling.IntervalMS = %d, want 5000", mgr.Config().Polling.IntervalMS)
	}
	if mgr.LastLoadError() == nil {
		t.Error("after failed Reload: LastLoadError() is nil, want non-nil")
	}
}

// TestManager_ReloadRetainsOnInvalidRetentionDays covers R5: a reload whose
// workspace.retention_days fails validation leaves the previously loaded
// configuration in force rather than disabling the bound or terminating
// the process.
func TestManager_ReloadRetainsOnInvalidRetentionDays(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, retentionWorkflow(30))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := mgr.Config().Workspace.RetentionDays; got != 30 {
		t.Fatalf("initial Config().Workspace.RetentionDays = %d, want 30", got)
	}

	// 5 is in the rejected 1-29 range.
	mustWriteFile(t, path, retentionWorkflow(5))

	err = mgr.Reload()
	if err == nil {
		t.Fatal("Reload() error = nil, want error")
	}
	if got := mgr.Config().Workspace.RetentionDays; got != 30 {
		t.Errorf("after failed Reload: Config().Workspace.RetentionDays = %d, want 30 (retained)", got)
	}
	if mgr.LastLoadError() == nil {
		t.Error("after failed Reload: LastLoadError() is nil, want non-nil")
	}
}

// TestManager_WatchPicksUpChange verifies that workflow file changes are
// detected and trigger re-read/re-apply without restart.
func TestManager_WatchPicksUpChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	// Give the watcher time to register.
	time.Sleep(50 * time.Millisecond)

	writeWorkflow(t, path, validWorkflow(10000))

	ok := pollUntil(func() bool {
		return mgr.Config().Polling.IntervalMS == 10000
	})
	if !ok {
		t.Errorf("config not updated within timeout: Polling.IntervalMS = %d, want 10000",
			mgr.Config().Polling.IntervalMS)
	}
}

// TestManager_WatchInvalidRetainsGood verifies that an invalid workflow
// reload keeps the last known good effective configuration.
func TestManager_WatchInvalidRetainsGood(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	time.Sleep(50 * time.Millisecond)

	// Write invalid YAML.
	writeWorkflow(t, path, []byte("---\n[[[bad yaml\n---\nprompt\n"))

	// Wait until the reload actually fired — confirmed by LastLoadError becoming
	// set — then assert the last-known-good config was preserved.
	ok := pollUntil(func() bool {
		return mgr.LastLoadError() != nil
	})
	if !ok {
		t.Fatal("reload of invalid file was not detected within timeout")
	}

	if mgr.Config().Polling.IntervalMS != 5000 {
		t.Errorf("after invalid reload: Polling.IntervalMS = %d, want 5000",
			mgr.Config().Polling.IntervalMS)
	}
}

func TestManager_ConcurrentReadSafety(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	time.Sleep(50 * time.Millisecond)

	// Readers spin until the reload is confirmed to have completed, ensuring
	// concurrent access actually overlaps with the write under -race.
	var reloaded atomic.Bool
	var wg sync.WaitGroup
	const readers = 10

	for range readers {
		wg.Go(func() {
			for !reloaded.Load() {
				_ = mgr.Config()
				_ = mgr.PromptTemplate()
				_ = mgr.LastLoadError()
			}
		})
	}

	writeWorkflow(t, path, validWorkflow(7777))

	ok := pollUntil(func() bool {
		if mgr.Config().Polling.IntervalMS == 7777 {
			reloaded.Store(true)
			return true
		}
		return false
	})
	wg.Wait()

	if !ok {
		t.Error("config not updated within timeout: Polling.IntervalMS did not reach 7777")
	}
}

func TestManager_DebounceCoalescence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(1000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	time.Sleep(50 * time.Millisecond)

	// Write 5 times in rapid succession.
	for i := range 5 {
		val := 2000 + i*1000 // 2000, 3000, 4000, 5000, 6000
		writeWorkflow(t, path, validWorkflow(val))
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for debounce + reload.
	ok := pollUntil(func() bool {
		return mgr.Config().Polling.IntervalMS == 6000
	})
	if !ok {
		t.Errorf("debounce did not coalesce to last value: Polling.IntervalMS = %d, want 6000",
			mgr.Config().Polling.IntervalMS)
	}
}

func TestManager_DeleteAndRecreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	time.Sleep(50 * time.Millisecond)

	// Simulate editor delete-and-recreate.
	mustRemove(t, path)
	time.Sleep(50 * time.Millisecond)
	writeWorkflow(t, path, validWorkflow(8888))

	ok := pollUntil(func() bool {
		return mgr.Config().Polling.IntervalMS == 8888
	})
	if !ok {
		t.Errorf("after delete+recreate: Polling.IntervalMS = %d, want 8888",
			mgr.Config().Polling.IntervalMS)
	}

	// Confirm watcher is still alive — write a third value.
	writeWorkflow(t, path, validWorkflow(9999))

	ok = pollUntil(func() bool {
		return mgr.Config().Polling.IntervalMS == 9999
	})
	if !ok {
		t.Errorf("after second write: Polling.IntervalMS = %d, want 9999",
			mgr.Config().Polling.IntervalMS)
	}
}

func TestManager_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	// Stop should not hang. If it does, the test will time out.
	done := make(chan struct{})
	go func() {
		mgr.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s after context cancellation")
	}
}

func TestManager_StopIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Double stop should not panic.
	mgr.Stop()
	mgr.Stop()
}

func TestManager_RecoverAfterInvalidReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	time.Sleep(50 * time.Millisecond)

	// Write invalid content.
	writeWorkflow(t, path, []byte("---\n[[[bad\n---\nprompt\n"))
	time.Sleep(300 * time.Millisecond)

	if mgr.Config().Polling.IntervalMS != 5000 {
		t.Fatalf("after invalid reload: Polling.IntervalMS = %d, want 5000",
			mgr.Config().Polling.IntervalMS)
	}

	// Now write valid content again — watcher should recover.
	writeWorkflow(t, path, validWorkflow(7777))

	ok := pollUntil(func() bool {
		return mgr.Config().Polling.IntervalMS == 7777
	})
	if !ok {
		t.Errorf("recovery after invalid reload failed: Polling.IntervalMS = %d, want 7777",
			mgr.Config().Polling.IntervalMS)
	}
	if mgr.LastLoadError() != nil {
		t.Errorf("after recovery: LastLoadError = %v, want nil", mgr.LastLoadError())
	}
}

// workflowWithStates returns valid WORKFLOW.md content with the given
// active and terminal state lists. An empty slice results in the key being
// absent from the front matter.
func workflowWithStates(active, terminal []string) []byte {
	var s strings.Builder
	s.WriteString("---\npolling:\n  interval_ms: 5000\ntracker:\n")
	if len(active) > 0 {
		s.WriteString("  active_states:\n")
		for _, st := range active {
			fmt.Fprintf(&s, "    - %s\n", st)
		}
	}
	if len(terminal) > 0 {
		s.WriteString("  terminal_states:\n")
		for _, st := range terminal {
			fmt.Fprintf(&s, "    - %s\n", st)
		}
	}
	s.WriteString("---\nDo the task for {{ .issue.title }}.\n")
	return []byte(s.String())
}

// rejectBothEmpty is a ValidateFunc that rejects configs where both
// active_states and terminal_states are empty.
func rejectBothEmpty(cfg config.ServiceConfig) error {
	if len(cfg.Tracker.ActiveStates) == 0 && len(cfg.Tracker.TerminalStates) == 0 {
		return errors.New("both state lists empty")
	}
	return nil
}

func TestManager_ReloadValidatorRejects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with both state lists empty.
	mustWriteFile(t, path, workflowWithStates(nil, nil))

	err = mgr.Reload()
	if err == nil {
		t.Fatal("Reload() error = nil, want error from validator")
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 1 || got[0] != "To Do" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [To Do]", got)
	}
	if mgr.LastLoadError() == nil {
		t.Error("LastLoadError() = nil, want non-nil")
	}
}

func TestManager_ReloadValidatorAccepts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with different but valid state lists.
	mustWriteFile(t, path, workflowWithStates([]string{"In Progress"}, []string{"Closed"}))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 1 || got[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", got)
	}
	if mgr.LastLoadError() != nil {
		t.Errorf("LastLoadError() = %v, want nil", mgr.LastLoadError())
	}
}

func TestManager_ReloadWithoutValidatorPromotesBothEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with both state lists empty — no validator so should promote.
	mustWriteFile(t, path, workflowWithStates(nil, nil))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 0 {
		t.Errorf("Config().Tracker.ActiveStates = %v, want empty", got)
	}
}

func TestNewManager_ValidatorRejectsInitialLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates(nil, nil))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err == nil {
		t.Fatal("NewManager() error = nil, want error from validator")
	}
	if mgr != nil {
		t.Errorf("NewManager() returned non-nil Manager on validation failure")
	}
}

func TestManager_ReloadEmptyActiveNonEmptyTerminalPromotes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Empty active_states but non-empty terminal_states — should pass.
	mustWriteFile(t, path, workflowWithStates(nil, []string{"Done"}))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.TerminalStates; len(got) != 1 || got[0] != "Done" {
		t.Errorf("Config().Tracker.TerminalStates = %v, want [Done]", got)
	}
}

func TestManager_ReloadNonEmptyActiveEmptyTerminalPromotes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Non-empty active_states but empty terminal_states — should pass.
	mustWriteFile(t, path, workflowWithStates([]string{"In Progress"}, nil))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 1 || got[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", got)
	}
}

func TestManager_SetLogger(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	var bufA, bufB bytes.Buffer
	loggerA := slog.New(slog.NewTextHandler(&bufA, &slog.HandlerOptions{Level: slog.LevelDebug}))
	loggerB := slog.New(slog.NewTextHandler(&bufB, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mgr, err := NewManager(path, loggerA)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.SetLogger(loggerB)

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	writeWorkflow(t, path, []byte("---\n[[[invalid\n---\nprompt\n"))

	ok := pollUntil(func() bool { return mgr.LastLoadError() != nil })
	if !ok {
		t.Fatal("reload of invalid file was not detected within timeout")
	}

	// Stop before reading buffers to ensure the watcher goroutine has exited
	// and no concurrent writes remain.
	mgr.Stop()

	if !strings.Contains(bufB.String(), "workflow reload failed") {
		t.Errorf("loggerB output does not contain %q: %s", "workflow reload failed", bufB.String())
	}
	if strings.Contains(bufA.String(), "workflow reload failed") {
		t.Errorf("loggerA output unexpectedly contains %q: %s", "workflow reload failed", bufA.String())
	}
}

func TestManager_SetLoggerNil(t *testing.T) {
	// No t.Parallel — this test mutates the global slog.Default.
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	var explicitBuf, defaultBuf bytes.Buffer
	explicitLogger := slog.New(slog.NewTextHandler(&explicitBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defaultLogger := slog.New(slog.NewTextHandler(&defaultBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	prevDefault := slog.Default()
	slog.SetDefault(defaultLogger)
	defer slog.SetDefault(prevDefault)

	mgr, err := NewManager(path, explicitLogger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.SetLogger(nil)

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	writeWorkflow(t, path, []byte("---\n[[[invalid\n---\nprompt\n"))

	ok := pollUntil(func() bool { return mgr.LastLoadError() != nil })
	if !ok {
		t.Fatal("reload of invalid file was not detected within timeout")
	}

	mgr.Stop()

	if !strings.Contains(defaultBuf.String(), "workflow reload failed") {
		t.Errorf("default logger output does not contain %q: %s", "workflow reload failed", defaultBuf.String())
	}
	if strings.Contains(explicitBuf.String(), "workflow reload failed") {
		t.Errorf("explicit logger output unexpectedly contains %q: %s", "workflow reload failed", explicitBuf.String())
	}
}

func TestManager_SetLoggerConcurrentWithReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			mgr.SetLogger(testLogger())
		}
	})

	for i := range 5 {
		writeWorkflow(t, path, validWorkflow(5000+i))
		time.Sleep(40 * time.Millisecond)
	}

	stop.Store(true)
	wg.Wait()
	mgr.Stop()
}

// workflowWithDispatch returns WORKFLOW.md content with dispatch rules
// that reference template files that exist under dir.
func workflowWithDispatch(t *testing.T, dir string) []byte {
	t.Helper()
	tmplDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bugTmpl := filepath.Join(tmplDir, "bug.md")
	if err := os.WriteFile(bugTmpl, []byte("bug template {{ .issue.identifier }}"), 0o644); err != nil {
		t.Fatalf("write bug.md: %v", err)
	}
	return []byte(`---
polling:
  interval_ms: 5000
agent:
  kind: mock
dispatch:
  rules:
    - name: bug
      match:
        labels: ["bug"]
      template: prompts/bug.md
---
You are assigned to {{ .issue.identifier }}.
`)
}

func TestManager_PromptTemplateByID_EmptyIDReturnsBodyTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := mgr.PromptTemplateByID("")
	if got == nil {
		t.Fatal("PromptTemplateByID(\"\") = nil, want body template")
	}
	body := mgr.PromptTemplate()
	if got != body {
		t.Errorf("PromptTemplateByID(\"\") != PromptTemplate(): pointers differ")
	}
}

func TestManager_PromptTemplateByID_UnknownIDReturnsNil(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := mgr.PromptTemplateByID("/nonexistent/path.md")
	if got != nil {
		t.Errorf("PromptTemplateByID(\"/nonexistent/path.md\") = non-nil, want nil")
	}
}

func TestManager_PromptTemplateByID_KnownRuleTemplate(t *testing.T) {
	t.Parallel()

	// Canonicalize the temp dir via EvalSymlinks so the expected key
	// matches the resolved absolute path the manager registers (on
	// Windows, t.TempDir() may return an 8.3 short name that the
	// manager's symlink-evaluation step rewrites to the long form).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	content := workflowWithDispatch(t, dir)
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, content)

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The template is registered under its resolved absolute path.
	bugTmplAbs := filepath.Join(dir, "prompts", "bug.md")

	got := mgr.PromptTemplateByID(bugTmplAbs)
	if got == nil {
		t.Fatalf("PromptTemplateByID(%q) = nil, want non-nil", bugTmplAbs)
	}
}

// TestManager_PromptTemplateByID_CanonicalKeyAcrossSymlinks locks the
// invariant that the per-rule template index is keyed by the canonical,
// EvalSymlinks-resolved absolute path, even when the workflow file is
// reached through a symlinked workflow directory. Windows CI runners
// surfaced this contract via 8.3 short-name paths (RUNNER~1 → long
// form); creating an explicit symlink reproduces the same canonical-
// vs-raw drift on Linux and macOS so the regression is caught on every
// supported runner. Skips gracefully on hosts where os.Symlink is
// unsupported or forbidden.
func TestManager_PromptTemplateByID_CanonicalKeyAcrossSymlinks(t *testing.T) {
	t.Parallel()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()) error = %v, want nil", err)
	}
	canonical := filepath.Join(base, "real")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", canonical, err)
	}

	via := filepath.Join(base, "via")
	if err := os.Symlink(canonical, via); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("os.Symlink(%q -> %q) unsupported on this filesystem: %v", canonical, via, err)
		}
		t.Skipf("os.Symlink(%q -> %q) = %v (skipping symlink-based assertion)", canonical, via, err)
	}

	// Write the workflow and its template under the canonical
	// directory so the on-disk layout is independent of which path
	// (canonical or symlinked) the manager is asked to load from.
	content := workflowWithDispatch(t, canonical)
	mustWriteFile(t, filepath.Join(canonical, "WORKFLOW.md"), content)

	mgr, err := NewManager(filepath.Join(via, "WORKFLOW.md"), testLogger())
	if err != nil {
		t.Fatalf("NewManager(via=%q) error = %v, want nil", via, err)
	}

	wantKey := filepath.Join(canonical, "prompts", "bug.md")
	if got := mgr.PromptTemplateByID(wantKey); got == nil {
		t.Fatalf("PromptTemplateByID(canonical=%q) = nil, want non-nil", wantKey)
	}
}

func TestManager_WithAgentKindProbe_AcceptsKnownKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	// Use a workflow with agent kind "mock" — probe accepts it.
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 5000
agent:
  kind: mock
dispatch:
  rules:
    - name: test
      agent: mock
---
Prompt.
`))

	probe := func(kind string) bool { return kind == "mock" }
	mgr, err := NewManager(path, testLogger(), WithAgentKindProbe(probe))
	if err != nil {
		t.Fatalf("NewManager with probe: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewManager returned nil manager")
	}
}

func TestManager_WithAgentKindProbe_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	// Dispatch rule references "nonexistent-agent".
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 5000
agent:
  kind: mock
dispatch:
  rules:
    - name: bad
      agent: nonexistent-agent
---
Prompt.
`))

	probe := func(kind string) bool { return kind == "mock" }
	_, err := NewManager(path, testLogger(), WithAgentKindProbe(probe))
	if err == nil {
		t.Fatal("NewManager with rejecting probe = nil error, want error")
	}
}

func TestManager_FailSafeReload_DispatchError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	initialInterval := mgr.Config().Polling.IntervalMS
	initialPrompt := mgr.PromptTemplate()

	// Overwrite with an invalid dispatch glob that BuildDispatchConfig will reject.
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 9999
dispatch:
  rules:
    - name: bad-glob
      match:
        labels: ["[unclosed"]
      agent: mock
---
Prompt.
`))

	err = mgr.Reload()
	if err == nil {
		t.Fatal("Reload() error = nil, want error for invalid dispatch config")
	}

	if mgr.Config().Polling.IntervalMS != initialInterval {
		t.Errorf("after failed Reload: Polling.IntervalMS = %d, want %d (previous good value)",
			mgr.Config().Polling.IntervalMS, initialInterval)
	}
	if mgr.PromptTemplate() != initialPrompt {
		t.Errorf("after failed Reload: PromptTemplate pointer changed, want same previous good template")
	}
	if mgr.LastLoadError() == nil {
		t.Error("after failed Reload: LastLoadError() = nil, want non-nil")
	}
}

func TestManager_PerRuleTemplate_FrontMatterRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write a per-rule template that has front matter — must be rejected.
	tmplDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tmplPath := filepath.Join(tmplDir, "with_fm.md")
	if err := os.WriteFile(tmplPath, []byte("---\nkey: value\n---\nTemplate body."), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 5000
dispatch:
  rules:
    - name: with-fm
      match:
        labels: ["test"]
      template: prompts/with_fm.md
---
Prompt.
`))

	_, err := NewManager(path, testLogger())
	if err == nil {
		t.Fatal("NewManager with front-matter template = nil error, want error")
	}

	errMsg := err.Error()
	if !bytes.Contains([]byte(errMsg), []byte("front matter")) {
		t.Errorf("error = %q, want to contain %q", errMsg, "front matter")
	}
}

// labelCommandsWorkflow returns a WORKFLOW.md with an active
// reactions.label_commands block and the given prompt body.
func labelCommandsWorkflow(promptBody string) []byte {
	return fmt.Appendf(nil, "---\n"+
		"reactions:\n"+
		"  label_commands:\n"+
		"    provider: github\n"+
		"    review_label: \"sortie:review\"\n"+
		"---\n"+
		"%s\n", promptBody)
}

const labelReviewWarnMessage = "label_commands active but prompt template has no label_review branch"

// TestManager_WarnsWhenLabelReviewTokenMissing covers A12: with
// label_commands active and a prompt body that never references
// label_review, load emits a Warn; the same active block with a
// {{ if .label_review }} branch in the prompt suppresses it; and an
// inactive label_commands block suppresses it regardless of prompt
// content. The warning is advisory only — NewManager never fails because
// of it.
func TestManager_WarnsWhenLabelReviewTokenMissing(t *testing.T) {
	t.Parallel()

	t.Run("missing token warns", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, labelCommandsWorkflow("Do the task for {{ .issue.title }}."))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if !strings.Contains(buf.String(), labelReviewWarnMessage) {
			t.Errorf("logger output = %q, want the missing-token warning", buf.String())
		}
	})

	t.Run("token present suppresses the warning", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, labelCommandsWorkflow("{{ if .label_review }}review this{{ end }}\nDo the task for {{ .issue.title }}."))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if strings.Contains(buf.String(), labelReviewWarnMessage) {
			t.Errorf("logger output = %q, want no warning when the prompt references label_review", buf.String())
		}
	})

	t.Run("feature inactive suppresses the warning regardless of prompt content", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, validWorkflow(5000)) // no reactions.label_commands block

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if strings.Contains(buf.String(), labelReviewWarnMessage) {
			t.Errorf("logger output = %q, want no warning when label_commands is inactive", buf.String())
		}
	})
}

// labelFixCommandsWorkflow returns a WORKFLOW.md with an active
// reactions.label_commands block whose fix_label is set (review_label
// left at its default) and the given prompt body.
func labelFixCommandsWorkflow(promptBody string) []byte {
	return fmt.Appendf(nil, "---\n"+
		"reactions:\n"+
		"  label_commands:\n"+
		"    provider: github\n"+
		"    fix_label: \"sortie:fix\"\n"+
		"---\n"+
		"%s\n", promptBody)
}

// labelFixDisabledWorkflow returns a WORKFLOW.md with an active
// reactions.label_commands block whose fix_label is explicitly disabled
// (review_label left at its default, keeping the block valid) and the
// given prompt body.
func labelFixDisabledWorkflow(promptBody string) []byte {
	return fmt.Appendf(nil, "---\n"+
		"reactions:\n"+
		"  label_commands:\n"+
		"    provider: github\n"+
		"    fix_label: \"\"\n"+
		"---\n"+
		"%s\n", promptBody)
}

const labelFixWarnMessage = "label_commands active but prompt template has no label_fix branch"

// TestManager_WarnsWhenLabelFixTokenMissing covers A12: with
// label_commands active and fix_label set, a prompt body that never
// references label_fix produces a Warn on load; the same active block
// with a {{ if .label_fix }} branch in the prompt suppresses it; and an
// explicitly disabled fix_label suppresses it regardless of prompt
// content. The warning is advisory only — NewManager never fails
// because of it.
func TestManager_WarnsWhenLabelFixTokenMissing(t *testing.T) {
	t.Parallel()

	t.Run("missing token warns", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, labelFixCommandsWorkflow("Do the task for {{ .issue.title }}."))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if !strings.Contains(buf.String(), labelFixWarnMessage) {
			t.Errorf("logger output = %q, want the missing-token warning", buf.String())
		}
	})

	t.Run("token present suppresses the warning", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, labelFixCommandsWorkflow("{{ if .label_fix }}check out {{ .label_fix.branch }}{{ end }}\nDo the task for {{ .issue.title }}."))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if strings.Contains(buf.String(), labelFixWarnMessage) {
			t.Errorf("logger output = %q, want no warning when the prompt references label_fix", buf.String())
		}
	})

	t.Run("fix_label empty suppresses the warning regardless of prompt content", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, labelFixDisabledWorkflow("Do the task for {{ .issue.title }}."))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if strings.Contains(buf.String(), labelFixWarnMessage) {
			t.Errorf("logger output = %q, want no warning when fix_label is disabled", buf.String())
		}
	})

	t.Run("feature inactive suppresses the warning regardless of prompt content", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, validWorkflow(5000)) // no reactions.label_commands block

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		mgr, err := NewManager(path, logger)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		mgr.Stop()

		if strings.Contains(buf.String(), labelFixWarnMessage) {
			t.Errorf("logger output = %q, want no warning when label_commands is inactive", buf.String())
		}
	})

	t.Run("warning never fails the load", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, labelFixCommandsWorkflow("Do the task for {{ .issue.title }}."))

		mgr, err := NewManager(path, testLogger())
		if err != nil {
			t.Fatalf("NewManager with missing label_fix token: %v, want success (advisory warning only)", err)
		}
		mgr.Stop()

		if err := mgr.LastLoadError(); err != nil {
			t.Errorf("LastLoadError() = %v, want nil", err)
		}
	})
}
