package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
// matching the pattern used by many editors.
func writeWorkflow(t *testing.T, path string, content []byte) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".workflow.tmp")
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename temp to target: %v", err)
	}
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

// Section 17.1: Workflow file changes are detected and trigger
// re-read/re-apply without restart.
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

// Section 17.1: Invalid workflow reload keeps last known good effective
// configuration and emits an operator-visible error.
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
