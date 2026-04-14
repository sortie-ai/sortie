package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runTestTimeout is the context timeout used by tests that exercise the
// full run() startup sequence. These tests verify startup behavior
// (logging, DB creation, flag parsing) and do not need to wait for a
// poll cycle — the orchestrator shuts down as soon as the context
// expires. Keep this short to avoid a cumulative 100+ second test suite.
//
// Windows CI runners need a longer budget because the pure-Go SQLite
// driver (modernc.org/sqlite) performs slower disk I/O on NTFS, and
// schema migrations can exceed the default 500 ms deadline.
var runTestTimeout = func() time.Duration {
	if runtime.GOOS == "windows" {
		return 10 * time.Second
	}
	return 500 * time.Millisecond
}()

// minimalWorkflow returns a minimal valid WORKFLOW.md content that
// includes tracker (file) and agent (mock) config needed for the
// full startup sequence.
func minimalWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
---
Do {{ .issue.title }}.
`)
}

// writeWorkflowFile creates a WORKFLOW.md in dir and returns its absolute path.
func writeWorkflowFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, minimalWorkflow(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeIssuesFixture creates a minimal issues.json fixture in dir for
// the file tracker adapter.
func writeIssuesFixture(t *testing.T, dir string) {
	t.Helper()
	issues := []map[string]any{
		{
			"id": "10001", "identifier": "PROJ-1",
			"title": "Test issue", "state": "To Do",
			"priority": 1, "labels": []string{},
			"comments": []any{}, "blocked_by": []any{},
		},
	}
	data, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "issues.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// freePort returns a TCP port number that is currently unbound on the
// loopback interface. There is an inherent race between the close and
// the subsequent Listen call in the code under test; this is acceptable
// for unit tests where contention is negligible.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() //nolint:errcheck // best-effort close
	return port
}

// setupRunDir creates a temp directory with WORKFLOW.md and issues.json
// fixture, sets CWD to that directory, and returns the workflow path.
func setupRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	return writeWorkflowFile(t, dir)
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q", out, "sortie "+Version)
	}
	if !strings.Contains(out, "commit:") {
		t.Errorf("stdout = %q, want to contain %q", out, "commit:")
	}
	if !strings.Contains(out, runtime.Version()) {
		t.Errorf("stdout = %q, want to contain %q", out, runtime.Version())
	}
}

func TestRunDumpVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"-dumpversion"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("stdout = %q, want %q", got, Version)
	}
}

func TestRunDumpVersionOverridesVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version", "-dumpversion"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("-dumpversion should take precedence; stdout = %q, want %q", got, Version)
	}
}

func TestRunVersionIgnoresWorkflowPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version", "/nonexistent/workflow.md"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--unknown"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("stderr should contain error message")
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on error", stdout.String())
	}
}

func TestRunTooManyArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"a", "b"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "too many arguments") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "too many arguments")
	}
}

func TestRunNonexistentPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{"/nonexistent/workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
}

func TestRunMissingDefaultWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
}

func TestRunValidWorkflowWithTimeout(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunAlreadyCancelledContext(t *testing.T) {
	// With a pre-cancelled context, the DB open fails immediately.
	// The startup sequence correctly returns exit code 1.
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
}

func TestRunPortFlagLogged(t *testing.T) {
	wfPath := setupRunDir(t)
	port := freePort(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--port", strconv.Itoa(port), wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "server_addr=") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "server_addr=")
	}
	if !strings.Contains(stderr.String(), "http server listening") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "http server listening")
	}
}

func quickStartWorkflow(issuesPath, workspaceRoot string) []byte {
	return []byte(fmt.Sprintf(`---
tracker:
  kind: file
  project: DEMO
  active_states:
    - "To Do"
  handoff_state: "Done"

file:
  path: %s

agent:
  kind: mock
  max_turns: 2

polling:
  interval_ms: 500

workspace:
  root: %s
---

Fix the following issue.

**{{ .issue.identifier }}**: {{ .issue.title }}

{{ .issue.description }}
`, issuesPath, workspaceRoot))
}

// quickStartIssues returns issues.json content matching the
// https://docs.sortie-ai.com/getting-started/quick-start/ tutorial.
func quickStartIssues() []byte {
	return []byte(`[
  {
    "id": "1",
    "identifier": "DEMO-1",
    "title": "Add input validation to signup form",
    "description": "The signup form accepts empty email addresses. Add validation before submission.",
    "state": "To Do",
    "priority": 1
  },
  {
    "id": "2",
    "identifier": "DEMO-2",
    "title": "Fix off-by-one error in pagination",
    "description": "Page 2 repeats the last item from page 1. The offset calculation is wrong.",
    "state": "To Do",
    "priority": 2
  }
]`)
}

// TestQuickStartScenario is an integration test that exercises the exact
// workflow described in https://docs.sortie-ai.com/getting-started/quick-start/ end-to-end:
// two issues dispatched with mock agent, two turns each, handoff to "Done".
func TestQuickStartScenario(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wsRoot := filepath.Join(dir, "workspaces")

	issuesPath := filepath.Join(dir, "issues.json")
	if err := os.WriteFile(issuesPath, quickStartIssues(), 0o644); err != nil {
		t.Fatal(err)
	}

	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, quickStartWorkflow(issuesPath, wsRoot), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr lockedBuf
	// 5 seconds: mock-agent turns complete in <1 s; polling interval is
	// 500 ms so the second tick confirming zero candidates arrives quickly.
	// Extra headroom avoids flakiness under -race in CI.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}

	logs := stderr.String()

	// Verify key lifecycle events from the quick-start scenario.
	checks := []struct {
		name   string
		substr string
	}{
		{"sortie started", `msg="sortie started"`},
		{"tick completed with 2 candidates", `msg="tick completed" candidates=2`},
		{"DEMO-1 session started", `msg="agent session started" issue_id=1 issue_identifier=DEMO-1`},
		{"DEMO-2 session started", `msg="agent session started" issue_id=2 issue_identifier=DEMO-2`},
		{"DEMO-1 turn 1 completed", `issue_identifier=DEMO-1 session_id=mock-session-001 turn_number=1`},
		{"DEMO-1 turn 2 completed", `issue_identifier=DEMO-1 session_id=mock-session-001 turn_number=2`},
		{"DEMO-2 turn 1 completed", `issue_identifier=DEMO-2 session_id=mock-session-001 turn_number=1`},
		{"DEMO-2 turn 2 completed", `issue_identifier=DEMO-2 session_id=mock-session-001 turn_number=2`},
		{"DEMO-1 worker exiting normally", `issue_identifier=DEMO-1 session_id=mock-session-001 exit_kind=normal`},
		{"DEMO-2 worker exiting normally", `issue_identifier=DEMO-2 session_id=mock-session-001 exit_kind=normal`},
		{"DEMO-1 handoff succeeded", `issue_identifier=DEMO-1 session_id=mock-session-001 handoff_state=Done`},
		{"DEMO-2 handoff succeeded", `issue_identifier=DEMO-2 session_id=mock-session-001 handoff_state=Done`},
		{"second tick finds zero candidates", `msg="tick completed" candidates=0`},
	}
	for _, c := range checks {
		if !strings.Contains(logs, c.substr) {
			t.Errorf("%s: expected log substring %q not found in output:\n%s", c.name, c.substr, logs)
		}
	}

	// .sortie.db must be created next to WORKFLOW.md.
	if _, err := os.Stat(filepath.Join(dir, ".sortie.db")); err != nil {
		t.Errorf("expected .sortie.db next to WORKFLOW.md: %v", err)
	}
}

// lockedBuf is a concurrency-safe bytes.Buffer for log capture in tests
// where background goroutines also write log output via slog.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *lockedBuf) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

func (lb *lockedBuf) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
}

func (e errWriter) Write(_ []byte) (int, error) { return 0, e.err }

func writeCustomWorkflowFile(t *testing.T, dir string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	absPath, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", p, err)
	}
	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return absPath
}

// noTrackerKindWorkflow is a minimal workflow with active/terminal
// states set (to pass ValidateConfigForPromotion) but tracker.kind
// absent (to trigger the preflight check).
func noTrackerKindWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

func TestRunSIGINTCleanShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT is not supported on Windows")
	}
	if os.Getenv("SORTIE_TEST_SIGINT_HELPER") == "1" {
		// --- subprocess ---
		// This code runs as a subprocess when the parent test injects the
		// env var. signal.NotifyContext handles SIGINT by cancelling ctx,
		// which causes run() to shut down cleanly.
		dir := os.Getenv("SORTIE_TEST_SIGINT_DIR")
		wfPath := filepath.Join(dir, "WORKFLOW.md")
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		os.Exit(run(ctx, []string{wfPath}, os.Stdout, os.Stderr))
		return // unreachable — silences staticcheck
	}

	// --- parent test ---
	dir := t.TempDir()
	writeIssuesFixture(t, dir)
	writeWorkflowFile(t, dir)

	cmd := exec.Command(os.Args[0], "-test.run=TestRunSIGINTCleanShutdown", "-test.v")
	cmd.Env = append(os.Environ(),
		"SORTIE_TEST_SIGINT_HELPER=1",
		"SORTIE_TEST_SIGINT_DIR="+dir,
	)
	var subStderr lockedBuf
	cmd.Stdout = io.Discard
	cmd.Stderr = &subStderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}

	// Poll subprocess stderr until "sortie started" appears — confirming
	// the orchestrator event loop is running before we send SIGINT.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(subStderr.String(), "sortie started") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(subStderr.String(), "sortie started") {
		cmd.Process.Kill() //nolint:errcheck // best-effort cleanup
		t.Fatalf("subprocess did not reach 'sortie started' within 5 s; stderr:\n%s", subStderr.String())
	}

	// Send SIGINT — should trigger context cancellation and clean shutdown.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("Signal(SIGINT): %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				t.Errorf("subprocess exited with code %d after SIGINT, want 0; stderr:\n%s",
					exitErr.ExitCode(), subStderr.String())
			} else {
				t.Errorf("subprocess Wait: %v; stderr:\n%s", waitErr, subStderr.String())
			}
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck // best-effort cleanup
		t.Errorf("subprocess did not exit within 5 s after SIGINT; stderr:\n%s", subStderr.String())
	}
}

// TestRunServerShutdownError covers the logger.Error("http server shutdown
// error", ...) branch that fires when srv.Shutdown returns an error because
// active connections are still open when the shutdown context expires.
//
// The test uses an incomplete HTTP request to hold a connection in the
// "active" state, preventing immediate shutdown, and a short
// serverShutdownTimeout override to make the context expire quickly.
func TestRunServerShutdownError(t *testing.T) {
	// No t.Parallel: mutates package-level serverShutdownTimeout.
	orig := serverShutdownTimeout
	serverShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serverShutdownTimeout = orig })

	wfPath := setupRunDir(t)

	var stderr lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := strconv.Itoa(freePort(t))
	result := make(chan int, 1)
	go func() {
		result <- run(ctx, []string{"--port", port, wfPath}, io.Discard, &stderr)
	}()

	// Wait until the HTTP server reports its bound address.
	var addr string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if log := stderr.String(); strings.Contains(log, "http server listening") {
			if i := strings.Index(log, "addr="); i >= 0 {
				rest := log[i+5:]
				if end := strings.IndexAny(rest, " \t\n\r"); end >= 0 {
					addr = rest[:end]
				} else {
					addr = strings.TrimSpace(rest)
				}
				// slog.TextHandler may quote string values (addr="host:port").
				addr = strings.Trim(addr, "\"")
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		cancel()
		t.Fatal("HTTP server did not start or log its address within 3 s")
	}

	// Open a TCP connection and send an incomplete HTTP request (no
	// trailing \r\n\r\n). The server goroutine is waiting to finish
	// reading the request headers, keeping the connection "active" from
	// http.Server.Shutdown's perspective.
	conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
	if dialErr != nil {
		cancel()
		t.Fatalf("dial %s: %v", addr, dialErr)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	//nolint:errcheck // test write — errors are unrecoverable here
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n"))

	// Give the server goroutine time to register the connection as active.
	time.Sleep(20 * time.Millisecond)

	// Cancel the run context to trigger the shutdown sequence.
	cancel()

	select {
	case code := <-result:
		// shutdown errors are logged but do not change the exit code.
		if code != 0 {
			t.Errorf("run() = %d, want 0 (shutdown error is non-fatal); stderr:\n%s",
				code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run() did not return within 3 s after context cancel; stderr:\n%s", stderr.String())
	}

	if !strings.Contains(stderr.String(), "http server shutdown error") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "http server shutdown error")
	}
}

// TestRunReadOnlyWorkflowDir covers the persistence.Open error path that
// fires when the database file cannot be created because the workflow
// directory has no write permission.
func TestRunReadOnlyWorkflowDir(t *testing.T) {
	// No t.Parallel: calls t.Chdir via setupRunDir, and mutates permissions.
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce POSIX file permission bits")
	}
	if os.Getuid() == 0 {
		t.Skip("skipping: root bypasses filesystem permission checks")
	}

	workflowDir := t.TempDir()
	writeIssuesFixture(t, workflowDir)
	writeWorkflowFile(t, workflowDir)
	t.Chdir(workflowDir)

	// Make the directory read-only: traversable and readable, but no writes.
	// This prevents creating .sortie.db while still allowing the workflow
	// file and issues fixture to be read by the startup sequence.
	if err := os.Chmod(workflowDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(workflowDir, 0o755) //nolint:errcheck // cleanup
	})

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	wfPath := filepath.Join(workflowDir, "WORKFLOW.md")
	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (DB must not be created in read-only dir); stderr: %s",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed to open database") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "failed to open database")
	}
}

func minimalWorkflowWithLogLevel(level string) []byte {
	return []byte(fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
logging:
  level: %s
---
Do {{ .issue.title }}.
`, level))
}

// writeWorkflowFileWithContent writes the given content as WORKFLOW.md
// in dir and returns its absolute path.
func writeWorkflowFileWithContent(t *testing.T, dir string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunLogLevelDebug(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-level", "debug", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "level=DEBUG")
	}
	if !strings.Contains(stderr.String(), "log_level=DEBUG") {
		t.Errorf("stderr = %q, want to contain %q (startup attr)", stderr.String(), "log_level=DEBUG")
	}
}

func TestRunLogLevelWarn(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-level", "warn", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	// INFO-level startup line must be suppressed at warn level.
	if strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want no INFO lines at warn level", stderr.String())
	}
}

func TestRunLogLevelInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--log-level", "bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `unknown log level "bogus"`) {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), `unknown log level "bogus"`)
	}
}

func TestRunLogLevelEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--log-level", ""}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown log level") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown log level")
	}
}

func TestRunLogLevelFromExtension(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogLevel("warn"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	// INFO-level lines must be suppressed when extension sets warn.
	if strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want no INFO lines when logging.level=warn", stderr.String())
	}
}

func TestRunLogLevelFlagOverridesExtension(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	// Extension requests error level; flag requests debug — flag must win.
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogLevel("error"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-level", "debug", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") {
		t.Errorf("stderr = %q, want to contain %q (flag wins over extension)", stderr.String(), "level=DEBUG")
	}
}

func TestRunLogLevelExtensionInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogLevel("bogus"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown log level") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown log level")
	}
}

func TestRunLogLevelDefault(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	// No --log-level flag and no extension — default is info.
	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want to contain INFO-level startup line", stderr.String())
	}
	// DEBUG-level lines must be absent at the default info level.
	if strings.Contains(stderr.String(), "level=DEBUG") {
		t.Errorf("stderr = %q, want no DEBUG lines at default info level", stderr.String())
	}
}

func TestRunLogLevelVersionIgnoredInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Version fast path must exit 0 even when --log-level is invalid.
	code := run(ctx, []string{"--version", "--log-level", "invalid"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (version fast path ignores invalid log level)", code)
	}
	if !strings.Contains(stdout.String(), "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q", stdout.String(), "sortie "+Version)
	}
}

func TestRunLogLevelDumpVersionIgnoredInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// -dumpversion fast path must exit 0 even when --log-level is invalid.
	code := run(ctx, []string{"-dumpversion", "--log-level", "invalid"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (-dumpversion fast path ignores invalid log level)", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("-dumpversion stdout = %q, want %q", got, Version)
	}
}

func TestRunExplicitPortServerAddr(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	port := freePort(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--port", strconv.Itoa(port), wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	want := "server_addr=127.0.0.1:" + strconv.Itoa(port)
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), want)
	}
}

func TestRunPortZeroDisablesServer(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--port", "0", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "server_addr") {
		t.Errorf("stderr = %q, must not contain %q (server is disabled)", stderr.String(), "server_addr")
	}
}

func TestRunCustomHost(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	port := freePort(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--host", "0.0.0.0", "--port", strconv.Itoa(port), wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	want := "server_addr=0.0.0.0:" + strconv.Itoa(port)
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), want)
	}
}

func TestRunDefaultPortInUseImplicit(t *testing.T) {
	// No t.Parallel: binds port 7678; cannot run concurrently with other
	// tests that use the default HTTP server port.
	wfPath := setupRunDir(t)

	ln, err := net.Listen("tcp", "127.0.0.1:7678")
	if err != nil {
		t.Skipf("cannot pre-bind port 7678 (already in use); skipping implicit-port test: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck // best-effort cleanup

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (graceful degradation on implicit default port); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "http server listen failed; running without HTTP server") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "http server listen failed; running without HTTP server")
	}
}

func TestRunExplicitPortInUse(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	occupiedPort := ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck // best-effort cleanup

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--port", strconv.Itoa(occupiedPort), wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "http server listen failed") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "http server listen failed")
	}
}

func TestRunInvalidHost(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--host", "invalid", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not a valid IP address") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "not a valid IP address")
	}
}

func minimalWorkflowWithLogFormat(format string) []byte {
	return []byte(fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
logging:
  format: %s
---
Do {{ .issue.title }}.
`, format))
}

func TestRunLogFormatJSON(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	foundStarting := false
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("stderr line %q is not valid JSON: %v", line, err)
			continue
		}
		if obj["msg"] == "sortie starting" {
			foundStarting = true
			if obj["log_format"] != "json" {
				t.Errorf("sortie starting line: log_format = %v, want %q", obj["log_format"], "json")
			}
		}
	}
	if !foundStarting {
		t.Errorf("stderr does not contain a JSON line with msg=%q", "sortie starting")
	}
}

func TestRunLogFormatText(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-format", "text", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "level=INFO")
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line != "" {
			if strings.HasPrefix(line, "{") {
				t.Errorf("first non-empty stderr line %q starts with '{', expected text format", line)
			}
			break
		}
	}
}

func TestRunLogFormatDefault(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	// No --log-format flag — default is text.
	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "level=INFO")
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line != "" {
			if strings.HasPrefix(line, "{") {
				t.Errorf("first non-empty stderr line %q starts with '{', want text format by default", line)
			}
			break
		}
	}
}

func TestRunLogFormatInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--log-format", "yaml"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `unknown log format "yaml"`) {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), `unknown log format "yaml"`)
	}
}

func TestRunLogFormatEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--log-format", ""}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown log format") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown log format")
	}
}

func TestRunLogFormatCaseInsensitive(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-format", "JSON", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("stderr line %q is not valid JSON: %v", line, err)
		}
	}
}

func TestRunLogFormatFromExtension(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogFormat("json"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("stderr line %q is not valid JSON: %v", line, err)
		}
	}
}

func TestRunLogFormatFlagOverridesExtension(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	// Extension requests json; flag requests text — flag must win.
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogFormat("json"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
	defer cancel()

	code := run(ctx, []string{"--log-format", "text", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want to contain %q (flag wins over extension)", stderr.String(), "level=INFO")
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line != "" {
			if strings.HasPrefix(line, "{") {
				t.Errorf("first non-empty stderr line %q starts with '{', want text format (flag wins)", line)
			}
			break
		}
	}
}

func TestRunLogFormatExtensionInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogFormat("yaml"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown log format") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown log format")
	}
}

func TestRunLogFormatExtensionNonStringType(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)

	// YAML integer 42 decodes as int — resolveLogFormat must reject it.
	content := []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
logging:
  format: 42
---
Do {{ .issue.title }}.
`)
	wfPath := writeWorkflowFileWithContent(t, dir, content)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid logging.format") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "invalid logging.format")
	}
}

func TestRunShortHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run([-h]) = %d, want 0", code)
	}
	if stdout.Len() == 0 {
		t.Fatal("run([-h]) stdout is empty, want help text")
	}
	if !strings.Contains(stdout.String(), "Turn issue tracker tickets") {
		t.Errorf("run([-h]) stdout = %q, want to contain %q", stdout.String(), "Turn issue tracker tickets")
	}
	if stderr.Len() != 0 {
		t.Errorf("run([-h]) stderr = %q, want empty", stderr.String())
	}
}

func TestRunShortVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"-V"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run([-V]) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "sortie ") {
		t.Errorf("run([-V]) stdout = %q, want to contain %q", stdout.String(), "sortie ")
	}
	if !strings.Contains(stdout.String(), "commit:") {
		t.Errorf("run([-V]) stdout = %q, want to contain %q", stdout.String(), "commit:")
	}
	if stderr.Len() != 0 {
		t.Errorf("run([-V]) stderr = %q, want empty", stderr.String())
	}
}

func TestRunLongHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run([--help]) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Turn issue tracker tickets") {
		t.Errorf("run([--help]) stdout = %q, want to contain %q", stdout.String(), "Turn issue tracker tickets")
	}
	if stderr.Len() != 0 {
		t.Errorf("run([--help]) stderr = %q, want empty", stderr.String())
	}
}
