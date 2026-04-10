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
	return []byte(fmt.Sprintf("---\npolling:\n  interval_ms: %d\n---\nDo the task for {{ .issue.title }}.\n", intervalMS))
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !reloaded.Load() {
				_ = mgr.Config()
				_ = mgr.PromptTemplate()
				_ = mgr.LastLoadError()
			}
		}()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	var s string
	s += "---\npolling:\n  interval_ms: 5000\ntracker:\n"
	if len(active) > 0 {
		s += "  active_states:\n"
		for _, st := range active {
			s += fmt.Sprintf("    - %s\n", st)
		}
	}
	if len(terminal) > 0 {
		s += "  terminal_states:\n"
		for _, st := range terminal {
			s += fmt.Sprintf("    - %s\n", st)
		}
	}
	s += "---\nDo the task for {{ .issue.title }}.\n"
	return []byte(s)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			mgr.SetLogger(testLogger())
		}
	}()

	for i := range 5 {
		writeWorkflow(t, path, validWorkflow(5000+i))
		time.Sleep(40 * time.Millisecond)
	}

	stop.Store(true)
	wg.Wait()
	mgr.Stop()
}
