package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// --- Closeable adapter spy infrastructure ---
//
// "closeable-file" and "closeable-mock" are test-only adapter kinds that
// wrap the real file tracker and mock agent adapters and additionally
// implement io.Closer. When Close() is called, the per-test atomic flag
// registered in trackerCloseFlags / agentCloseFlags is set to 1.
//
// Registration must happen exactly once per binary (registry.Register
// panics on duplicates), so sync.Once guards the call.

var closeableOnce sync.Once

// trackerCloseFlags and agentCloseFlags map a per-test token string to an
// *int32 that is atomically set to 1 when the respective adapter's
// Close() is called.
var (
	trackerCloseFlags sync.Map // token string -> *int32
	agentCloseFlags   sync.Map // token string -> *int32
)

// closeTokenSeq generates monotonically unique token strings.
var closeTokenSeq atomic.Int64

// registerCloseableAdapters registers the two test-only adapter kinds once.
func registerCloseableAdapters() {
	closeableOnce.Do(func() {
		registry.Trackers.Register("closeable-file", newCloseableFileTracker)
		registry.Agents.Register("closeable-mock", newCloseableMockAgent)
	})
}

// closeableFileTracker delegates all domain.TrackerAdapter calls to an
// inner file adapter and implements io.Closer.
type closeableFileTracker struct {
	domain.TrackerAdapter
	token string
}

// Close sets the per-test close flag atomically. Always returns nil.
func (c *closeableFileTracker) Close() error { //nolint:unparam // io.Closer requires error return; test double always succeeds
	if v, ok := trackerCloseFlags.Load(c.token); ok {
		atomic.StoreInt32(v.(*int32), 1)
	}
	return nil
}

func newCloseableFileTracker(cfg map[string]any) (domain.TrackerAdapter, error) {
	fileCtor, err := registry.Trackers.Get("file")
	if err != nil {
		return nil, fmt.Errorf("closeable-file: %w", err)
	}
	inner, err := fileCtor(cfg)
	if err != nil {
		return nil, err
	}
	token, _ := cfg["close_key"].(string)
	return &closeableFileTracker{TrackerAdapter: inner, token: token}, nil
}

// closeableMockAgent delegates all domain.AgentAdapter calls to an inner
// mock adapter and implements io.Closer.
type closeableMockAgent struct {
	domain.AgentAdapter
	token string
}

// Close sets the per-test close flag atomically. Always returns nil.
func (c *closeableMockAgent) Close() error { //nolint:unparam // io.Closer requires error return; test double always succeeds
	if v, ok := agentCloseFlags.Load(c.token); ok {
		atomic.StoreInt32(v.(*int32), 1)
	}
	return nil
}

func newCloseableMockAgent(cfg map[string]any) (domain.AgentAdapter, error) {
	mockCtor, err := registry.Agents.Get("mock")
	if err != nil {
		return nil, fmt.Errorf("closeable-mock: %w", err)
	}
	inner, err := mockCtor(cfg)
	if err != nil {
		return nil, err
	}
	token, _ := cfg["close_key"].(string)
	return &closeableMockAgent{AgentAdapter: inner, token: token}, nil
}

// newCloseToken allocates a unique token, stores two atomic flags in the
// package-level sync.Maps, and registers a t.Cleanup to remove them.
// Returns the token and pointers to the tracker and agent close flags.
func newCloseToken(t *testing.T) (token string, trackerFlag, agentFlag *int32) {
	t.Helper()
	token = fmt.Sprintf("close-test-%d", closeTokenSeq.Add(1))
	var tf, af int32
	trackerCloseFlags.Store(token, &tf)
	agentCloseFlags.Store(token, &af)
	t.Cleanup(func() {
		trackerCloseFlags.Delete(token)
		agentCloseFlags.Delete(token)
	})
	return token, &tf, &af
}

// writeCloseableWorkflow writes a WORKFLOW.md in dir that uses the
// "closeable-file" tracker and "closeable-mock" agent, both receiving
// token as the "close_key" extension so their Close() calls are
// observable via the package-level spy.
func writeCloseableWorkflow(t *testing.T, dir, issuesPath, token string) string {
	t.Helper()
	content := fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: closeable-file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: closeable-mock
closeable-file:
  path: %s
  close_key: %s
closeable-mock:
  close_key: %s
---
Do {{ .issue.title }}.
`, issuesPath, token, token)
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunAdapterCloseOnShutdown verifies that run() calls Close() on any
// adapter that implements io.Closer once the orchestrator's context is
// cancelled. Both the tracker and agent paths are exercised.
func TestRunAdapterCloseOnShutdown(t *testing.T) {
	registerCloseableAdapters()

	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)

	token, trackerFlag, agentFlag := newCloseToken(t)
	wfPath := writeCloseableWorkflow(t, dir, "issues.json", token)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
	}

	if got := atomic.LoadInt32(trackerFlag); got != 1 {
		t.Errorf("tracker adapter Close() not called after run() (flag = %d, want 1)", got)
	}
	if got := atomic.LoadInt32(agentFlag); got != 1 {
		t.Errorf("agent adapter Close() not called after run() (flag = %d, want 1)", got)
	}
}

// TestRunAdapterCloseNotCalledForNonClosers confirms that adapters which
// do not implement io.Closer are unaffected — run() must not panic or
// fail when the type assertion is false. This exercises the regular
// "file" + "mock" kinds used throughout the rest of the test suite.
func TestRunAdapterCloseNotCalledForNonClosers(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
	}
}
