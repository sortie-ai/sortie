package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- --dry-run flag tests ---

// assertNoDatabaseFile verifies no .sortie.db exists in workflowDir after a
// dry-run. Every dry-run test calls this to enforce the read-only invariant:
// --dry-run must never open or create the SQLite database.
func assertNoDatabaseFile(t *testing.T, workflowDir string) {
	t.Helper()
	dbPath := filepath.Join(workflowDir, ".sortie.db")
	_, err := os.Stat(dbPath)
	if err == nil {
		t.Errorf("database file %s must not exist after dry-run", dbPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed to stat database file %s: %v", dbPath, err)
	}
}

// minimalWorkflowWithSSH returns a minimal WORKFLOW.md with a worker SSH
// config: one host (host-a) capped at one concurrent agent per host.

// minimalWorkflowWithSSH returns a minimal WORKFLOW.md with a worker SSH
// config: one host (host-a) capped at one concurrent agent per host.
func minimalWorkflowWithSSH() []byte {
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
worker:
  ssh_hosts:
    - host-a
  max_concurrent_agents_per_host: 1
---
Do {{ .issue.title }}.
`)
}

// writeThreeIssueFixture writes a three-issue issues.json to dir for
// testing SSH host capacity limits.

// writeThreeIssueFixture writes a three-issue issues.json to dir for
// testing SSH host capacity limits.
func writeThreeIssueFixture(t *testing.T, dir string) {
	t.Helper()
	data := []byte(`[
{"id":"10001","identifier":"PROJ-1","title":"Issue 1","state":"To Do","labels":[],"comments":[],"blocked_by":[]},
{"id":"10002","identifier":"PROJ-2","title":"Issue 2","state":"To Do","labels":[],"comments":[],"blocked_by":[]},
{"id":"10003","identifier":"PROJ-3","title":"Issue 3","state":"To Do","labels":[],"comments":[],"blocked_by":[]}
]`)
	if err := os.WriteFile(filepath.Join(dir, "issues.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDryRunExitZero(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run: complete") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run: complete")
	}
	if !strings.Contains(stderr.String(), "dry-run: candidate") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run: candidate")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunNoCandidates(t *testing.T) {
	// No t.Parallel: uses t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "issues.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfPath := writeWorkflowFile(t, dir)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run, empty issues) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "candidates_fetched=0") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "candidates_fetched=0")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunNoDatabaseFile(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run) = %d, want 0; stderr: %s", code, stderr.String())
	}
	// Primary safety invariant: --dry-run must never open or create the database.
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunInvalidWorkflow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Unclosed inline sequence triggers a YAML parse error.
	invalid := []byte("---\n{key: [unclosed\n---\nDo {{ .issue.title }}.\n")
	wfPath := writeCustomWorkflowFile(t, dir, invalid)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(--dry-run, invalid YAML) = %d, want 1; stderr: %s", code, stderr.String())
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunPreflightFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, noTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(--dry-run, missing tracker.kind) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "preflight") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "preflight")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunWithLogLevel(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", "--log-level", "debug", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run --log-level debug) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run: complete") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run: complete")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunPortIgnored(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", "--port", "0", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run --port 0) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "http server listening") {
		t.Errorf("stderr = %q, must not contain %q (HTTP server must not start in dry-run mode)", stderr.String(), "http server listening")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunTrackerFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingPath := filepath.Join(dir, "does_not_exist.json")

	content := fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: %s
---
Do {{ .issue.title }}.
`, missingPath)
	wfPath := writeCustomWorkflowFile(t, dir, []byte(content))

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(--dry-run, missing issues file) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed to fetch") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "failed to fetch")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunWithVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// --version fast path must win over --dry-run, exiting 0 with the banner.
	code := run(ctx, []string{"--version", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--version --dry-run) = %d, want 0; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q (version banner)", stdout.String(), "sortie "+Version)
	}
}

func TestRunDryRunSSHHostCapacity(t *testing.T) {
	// No t.Parallel: uses t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)
	writeThreeIssueFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithSSH())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run, SSH config + 3 issues) = %d, want 0; stderr: %s", code, stderr.String())
	}

	logs := stderr.String()

	// With max_concurrent_agents_per_host=1, only the first candidate
	// acquires the SSH host; subsequent candidates are blocked.
	dispatched := strings.Count(logs, "would_dispatch=true")
	if dispatched > 1 {
		t.Errorf("would_dispatch=true count = %d, want at most 1 (SSH host capacity=1)", dispatched)
	}
	if !strings.Contains(logs, "ssh_hosts_at_capacity") {
		t.Errorf("stderr does not contain %q; full output:\n%s", "ssh_hosts_at_capacity", logs)
	}
	assertNoDatabaseFile(t, dir)
}

// --- GitHub validate tests ---

// githubInvalidProjectWorkflow is a minimal GitHub workflow where
// tracker.project is not in owner/repo format (no slash), used to
// trigger the tracker.project.format preflight diagnostic.

func TestRunDryRunNoServer(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "server_addr") {
		t.Errorf("stderr = %q, must not contain %q (no server in dry-run mode)", stderr.String(), "server_addr")
	}
	if !strings.Contains(stderr.String(), "dry-run") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run")
	}
}

// --- resolveLogFormat tests ---

// --- runDryRun blocker-gate tests ---

// dryRunFakeTracker is a configurable domain.TrackerAdapter double that
// returns a fixed candidate list, so runDryRun tests can drive specific
// blocker scenarios without a real tracker adapter.
type dryRunFakeTracker struct {
	issues []domain.Issue
}

var _ domain.TrackerAdapter = (*dryRunFakeTracker)(nil)

func (f *dryRunFakeTracker) FetchCandidateIssues(context.Context) ([]domain.Issue, error) {
	return f.issues, nil
}
func (f *dryRunFakeTracker) FetchIssueByID(context.Context, string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (f *dryRunFakeTracker) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (f *dryRunFakeTracker) FetchIssueStatesByIDs(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (f *dryRunFakeTracker) FetchIssueStatesByIdentifiers(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (f *dryRunFakeTracker) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	return nil, nil
}
func (f *dryRunFakeTracker) TransitionIssue(context.Context, string, string) error { return nil }
func (f *dryRunFakeTracker) CommentIssue(context.Context, string, string) error    { return nil }
func (f *dryRunFakeTracker) AddLabel(context.Context, string, string) error        { return nil }

// dryRunFakeResolver is a configurable orchestrator.BlockerResolver
// double, recording every Resolve call in the order runDryRun makes
// them.
type dryRunFakeResolver struct {
	needsReadFn func(domain.Issue) bool
	resolveFn   func(context.Context, domain.Issue) (domain.Issue, error)
	calls       []string
}

func (f *dryRunFakeResolver) NeedsRead(issue domain.Issue) bool {
	if f.needsReadFn == nil {
		return false
	}
	return f.needsReadFn(issue)
}

func (f *dryRunFakeResolver) Resolve(ctx context.Context, issue domain.Issue) (domain.Issue, error) {
	f.calls = append(f.calls, issue.ID)
	if f.resolveFn == nil {
		return issue, nil
	}
	return f.resolveFn(ctx, issue)
}

// discardStdoutLogger returns a text logger writing to w at Info
// level, matching the level runDryRun's own records need to be
// captured at.
func discardStdoutLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}

// dryRunTestConfig returns a minimal config.ServiceConfig suitable for
// direct runDryRun calls, with active/terminal states and a
// caller-supplied concurrency cap.
func dryRunTestConfig(maxConcurrentAgents int) config.ServiceConfig {
	return config.ServiceConfig{
		Tracker: config.TrackerConfig{
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling: config.PollingConfig{IntervalMS: 30000},
		Agent:   config.AgentConfig{MaxConcurrentAgents: maxConcurrentAgents},
	}
}

func TestRunDryRun_SkipReasonBlockedBy(t *testing.T) {
	t.Parallel()

	tracker := &dryRunFakeTracker{issues: []domain.Issue{
		{
			ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
			BlockedBy: []domain.BlockerRef{{ID: "b", State: "To Do"}},
		},
	}}

	var stderr bytes.Buffer
	logger := discardStdoutLogger(&stderr)

	code := runDryRun(context.Background(), dryRunTestConfig(10), logger, tracker, nil)
	if code != 0 {
		t.Fatalf("runDryRun = %d, want 0", code)
	}

	got := stderr.String()
	if !strings.Contains(got, "skip_reason=blocked_by") {
		t.Errorf("stderr missing skip_reason=blocked_by: %s", got)
	}
	if !strings.Contains(got, "would_dispatch=false") {
		t.Errorf("stderr missing would_dispatch=false: %s", got)
	}
}

func TestRunDryRun_SkipReasonBlockersUnresolved(t *testing.T) {
	t.Parallel()

	transientErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport}
	tracker := &dryRunFakeTracker{issues: []domain.Issue{
		{ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true},
	}}
	resolver := &dryRunFakeResolver{
		needsReadFn: func(domain.Issue) bool { return true },
		resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
			issue.BlockersUnresolved = true
			return issue, transientErr
		},
	}

	var stderr bytes.Buffer
	logger := discardStdoutLogger(&stderr)

	code := runDryRun(context.Background(), dryRunTestConfig(10), logger, tracker, resolver)
	if code != 0 {
		t.Fatalf("runDryRun = %d, want 0", code)
	}

	got := stderr.String()
	if !strings.Contains(got, "skip_reason=blockers_unresolved") {
		t.Errorf("stderr missing skip_reason=blockers_unresolved: %s", got)
	}
	if len(resolver.calls) != 1 {
		t.Errorf("resolver calls = %d, want 1", len(resolver.calls))
	}
}

func TestRunDryRun_SkipReasonBlockersIncomplete(t *testing.T) {
	t.Parallel()

	tracker := &dryRunFakeTracker{issues: []domain.Issue{
		{ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true},
	}}
	resolver := &dryRunFakeResolver{
		needsReadFn: func(domain.Issue) bool { return false },
	}

	var stderr bytes.Buffer
	logger := discardStdoutLogger(&stderr)

	code := runDryRun(context.Background(), dryRunTestConfig(10), logger, tracker, resolver)
	if code != 0 {
		t.Fatalf("runDryRun = %d, want 0", code)
	}

	got := stderr.String()
	if !strings.Contains(got, "skip_reason=blockers_incomplete") {
		t.Errorf("stderr missing skip_reason=blockers_incomplete: %s", got)
	}
	if len(resolver.calls) != 0 {
		t.Errorf("resolver calls = %d, want 0 (NeedsRead reported false)", len(resolver.calls))
	}
}

// TestRunDryRun_CapacityFullMakesNoBlockerRead pins that capacity is
// evaluated before EvaluateCandidate, so a candidate with no free slot
// spends no blocker read.
func TestRunDryRun_CapacityFullMakesNoBlockerRead(t *testing.T) {
	t.Parallel()

	tracker := &dryRunFakeTracker{issues: []domain.Issue{
		{ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true},
	}}
	resolver := &dryRunFakeResolver{
		needsReadFn: func(domain.Issue) bool { return true },
	}

	var stderr bytes.Buffer
	logger := discardStdoutLogger(&stderr)

	// Zero concurrency budget: no candidate ever has a free slot.
	code := runDryRun(context.Background(), dryRunTestConfig(0), logger, tracker, resolver)
	if code != 0 {
		t.Fatalf("runDryRun = %d, want 0", code)
	}

	if len(resolver.calls) != 0 {
		t.Errorf("resolver calls = %d, want 0: a capacity-full candidate must not spend a blocker read", len(resolver.calls))
	}
	if !strings.Contains(stderr.String(), "would_dispatch=false") {
		t.Errorf("stderr missing would_dispatch=false: %s", stderr.String())
	}
}

// TestRunDryRun_ReadBudgetPerCycle pins that a single dry-run cycle
// issues at most the per-pass blocker-read budget, holding the
// remainder as blockers_not_read.
func TestRunDryRun_ReadBudgetPerCycle(t *testing.T) {
	t.Parallel()

	const needyCount = 6
	issues := make([]domain.Issue, needyCount)
	for i := range issues {
		id := fmt.Sprintf("N-%d", i+1)
		issues[i] = domain.Issue{ID: id, Identifier: id, Title: "T", State: "To Do", BlockersUnresolved: true}
	}
	tracker := &dryRunFakeTracker{issues: issues}
	resolver := &dryRunFakeResolver{
		needsReadFn: func(issue domain.Issue) bool { return issue.BlockersUnresolved },
		resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
			issue.BlockersUnresolved = false
			return issue, nil
		},
	}

	var stderr bytes.Buffer
	logger := discardStdoutLogger(&stderr)

	code := runDryRun(context.Background(), dryRunTestConfig(10), logger, tracker, resolver)
	if code != 0 {
		t.Fatalf("runDryRun = %d, want 0", code)
	}

	if len(resolver.calls) == 0 || len(resolver.calls) >= needyCount {
		t.Fatalf("resolver calls = %d, want a positive count below %d (the budget must bind for this fixture)", len(resolver.calls), needyCount)
	}
	if got := strings.Count(stderr.String(), "skip_reason=blockers_not_read"); got != needyCount-len(resolver.calls) {
		t.Errorf("blockers_not_read count = %d, want %d", got, needyCount-len(resolver.calls))
	}
}
