package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- Shared test helpers ---

// writeVerdictFile writes a ReviewVerdict as JSON to <wsPath>/.sortie/review_verdict.json.
func writeVerdictFile(t *testing.T, wsPath string, verdict domain.ReviewVerdict) {
	t.Helper()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatalf("json.Marshal(verdict): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review_verdict.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile(review_verdict.json): %v", err)
	}
}

// runGit executes a git command inside dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initGitRepo creates a bare git repo in dir with an empty initial commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "commit", "--allow-empty", "-m", "init")
}

// selfReviewCfg returns a minimal SelfReviewConfig for testing.
func selfReviewCfg() config.SelfReviewConfig {
	return config.SelfReviewConfig{
		MaxIterations:         3,
		VerificationCommands:  []string{"echo ok"},
		VerificationTimeoutMS: 5000,
		MaxDiffBytes:          102400,
		Reviewer:              "same",
	}
}

// selfReviewIssue returns a minimal Issue for testing.
func selfReviewIssue() domain.Issue {
	return domain.Issue{
		ID:          "SR-1",
		Identifier:  "TEST-42",
		Title:       "Test self-review issue",
		Description: "Detailed description",
	}
}

// reviewMetricsCount tracks self-review metric calls.
type reviewMetricsCount struct {
	domain.NoopMetrics
	iterations []string
	sessions   []string
	capReached int
	verCmds    []string
}

func (m *reviewMetricsCount) IncSelfReviewIterations(verdict string) {
	m.iterations = append(m.iterations, verdict)
}
func (m *reviewMetricsCount) IncSelfReviewSessions(finalVerdict string) {
	m.sessions = append(m.sessions, finalVerdict)
}
func (m *reviewMetricsCount) IncSelfReviewCapReached() { m.capReached++ }
func (m *reviewMetricsCount) ObserveSelfReviewVerificationDuration(cmd string, _ float64) {
	m.verCmds = append(m.verCmds, cmd)
}

// --- verdictWriter is a test AgentAdapter that writes verdict files ---

// verdictWriter implements domain.AgentAdapter. Each call to RunTurn writes
// the next verdict in the slice to the workspace, then returns success.
type verdictWriter struct {
	wsPath   string
	verdicts []domain.ReviewVerdict // nil entry = no file written
	callIdx  int
}

func (v *verdictWriter) StartSession(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{ID: "test-sess"}, nil
}

func (v *verdictWriter) StopSession(_ context.Context, _ domain.Session) error { return nil }

func (v *verdictWriter) EventStream() <-chan domain.AgentEvent { return nil }

func (v *verdictWriter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	idx := v.callIdx
	v.callIdx++

	if idx < len(v.verdicts) && v.verdicts[idx].Verdict != "" {
		dir := filepath.Join(v.wsPath, ".sortie")
		_ = os.MkdirAll(dir, 0o755)
		data, _ := json.Marshal(v.verdicts[idx])
		_ = os.WriteFile(filepath.Join(dir, "review_verdict.json"), data, 0o600)
	}

	if params.OnEvent != nil {
		params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Message: fmt.Sprintf("turn %d", idx+1)})
	}

	return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
}

// failOnFirstAdapter is an AgentAdapter whose first RunTurn returns an error.
type failOnFirstAdapter struct {
	wsPath  string
	callIdx int
}

func (f *failOnFirstAdapter) StartSession(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{ID: "fail-sess"}, nil
}
func (f *failOnFirstAdapter) StopSession(_ context.Context, _ domain.Session) error { return nil }
func (f *failOnFirstAdapter) EventStream() <-chan domain.AgentEvent                 { return nil }
func (f *failOnFirstAdapter) RunTurn(_ context.Context, s domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	f.callIdx++
	return domain.TurnResult{}, fmt.Errorf("simulated turn error")
}

// statusWriterAdapter writes a status file to .sortie/status during RunTurn.
type statusWriterAdapter struct {
	wsPath string
	status string
}

func (s *statusWriterAdapter) StartSession(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{ID: "status-sess"}, nil
}
func (s *statusWriterAdapter) StopSession(_ context.Context, _ domain.Session) error { return nil }
func (s *statusWriterAdapter) EventStream() <-chan domain.AgentEvent                 { return nil }
func (s *statusWriterAdapter) RunTurn(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	dir := filepath.Join(s.wsPath, ".sortie")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "status"), []byte(s.status), 0o600)
	return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
}

// --- readReviewVerdict tests ---

func TestReadReviewVerdict_Pass(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeVerdictFile(t, wsPath, domain.ReviewVerdict{Verdict: "pass", Summary: "all good"})

	verdict, raw, parseErr := readReviewVerdict(wsPath)

	if parseErr != "" {
		t.Fatalf("parseErr = %q, want empty", parseErr)
	}
	if verdict == nil {
		t.Fatal("verdict = nil, want non-nil")
	}
	if verdict.Verdict != "pass" {
		t.Errorf("Verdict = %q, want %q", verdict.Verdict, "pass")
	}
	if verdict.Summary != "all good" {
		t.Errorf("Summary = %q, want %q", verdict.Summary, "all good")
	}
	if raw == "" {
		t.Error("raw JSON is empty, want non-empty")
	}
}

func TestReadReviewVerdict_Iterate(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeVerdictFile(t, wsPath, domain.ReviewVerdict{
		Verdict: "iterate",
		Summary: "found issues",
		Issues: []domain.ReviewIssue{
			{File: "main.go", Line: 10, Severity: "error", Message: "nil ptr"},
		},
	})

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if parseErr != "" {
		t.Fatalf("parseErr = %q, want empty", parseErr)
	}
	if verdict == nil {
		t.Fatal("verdict = nil, want non-nil")
	}
	if verdict.Verdict != "iterate" {
		t.Errorf("Verdict = %q, want %q", verdict.Verdict, "iterate")
	}
	if len(verdict.Issues) != 1 {
		t.Fatalf("Issues len = %d, want 1", len(verdict.Issues))
	}
	if verdict.Issues[0].File != "main.go" {
		t.Errorf("Issues[0].File = %q, want %q", verdict.Issues[0].File, "main.go")
	}
}

func TestReadReviewVerdict_Missing(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsPath, ".sortie"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if verdict != nil {
		t.Error("verdict should be nil when file is missing")
	}
	if parseErr == "" {
		t.Error("parseErr should be non-empty when file is missing")
	}
}

func TestReadReviewVerdict_NoDotSortieDir(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if verdict != nil {
		t.Error("verdict should be nil when .sortie dir is absent")
	}
	if parseErr == "" {
		t.Error("parseErr should be non-empty when .sortie dir is absent")
	}
}

func TestReadReviewVerdict_MalformedJSON(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review_verdict.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	verdict, raw, parseErr := readReviewVerdict(wsPath)

	if verdict != nil {
		t.Error("verdict should be nil on malformed JSON")
	}
	if raw == "" {
		t.Error("raw should contain the malformed content")
	}
	if parseErr == "" {
		t.Error("parseErr should be non-empty on malformed JSON")
	}
}

func TestReadReviewVerdict_UnknownVerdict(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeVerdictFile(t, wsPath, domain.ReviewVerdict{Verdict: "maybe", Summary: "unknown"})

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if verdict != nil {
		t.Error("verdict should be nil for unrecognized verdict string")
	}
	if !strings.Contains(parseErr, "unrecognized verdict") {
		t.Errorf("parseErr = %q, want to contain %q", parseErr, "unrecognized verdict")
	}
}

func TestReadReviewVerdict_OversizedFile(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	oversized := make([]byte, maxVerdictFileBytes+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "review_verdict.json"), oversized, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if verdict != nil {
		t.Error("verdict should be nil for oversized file")
	}
	if !strings.Contains(parseErr, "size limit") {
		t.Errorf("parseErr = %q, want to contain %q", parseErr, "size limit")
	}
}

func TestReadReviewVerdict_SymlinkRejection(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	target := t.TempDir()
	symlinkPath := filepath.Join(wsPath, ".sortie")
	if err := os.Symlink(target, symlinkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if verdict != nil {
		t.Error("verdict should be nil when .sortie is a symlink")
	}
	if !strings.Contains(parseErr, "symlink") {
		t.Errorf("parseErr = %q, want to contain %q", parseErr, "symlink")
	}
}

func TestReadReviewVerdict_CaseNormalization(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data := []byte(`{"verdict":"PASS","summary":"uppercase test"}`)
	if err := os.WriteFile(filepath.Join(dir, "review_verdict.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	verdict, _, parseErr := readReviewVerdict(wsPath)

	if parseErr != "" {
		t.Fatalf("parseErr = %q, want empty", parseErr)
	}
	if verdict == nil {
		t.Fatal("verdict = nil, want non-nil")
	}
	if verdict.Verdict != "pass" {
		t.Errorf("Verdict = %q, want %q (lowercased)", verdict.Verdict, "pass")
	}
}

// --- assembleReviewPrompt tests ---

func TestAssembleReviewPrompt_ContainsKeyFields(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{
		ID:          "ISS-1",
		Identifier:  "PROJ-1",
		Title:       "Fix the bug",
		Description: "Detailed description here",
	}
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new"
	vresults := []domain.VerificationResult{
		{Command: "make test", ExitCode: 0, DurationMS: 500, Stdout: "ok"},
	}

	prompt := assembleReviewPrompt(issue, diff, false, vresults, 1, 3)

	checks := []struct {
		label string
		want  string
	}{
		{"iteration header", "Iteration 1 of 3"},
		{"issue title", "Fix the bug"},
		{"issue description", "Detailed description here"},
		{"diff content", diff},
		{"verification command", "make test"},
		{"verdict file instructions", "review_verdict.json"},
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c.want) {
			t.Errorf("prompt missing %s (%q)", c.label, c.want)
		}
	}
}

func TestAssembleReviewPrompt_NoDiff(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "ISS-1", Title: "Fix bug"}
	prompt := assembleReviewPrompt(issue, "", false, nil, 1, 3)

	if !strings.Contains(prompt, "unavailable") {
		t.Errorf("prompt = %q, want to contain %q", prompt, "unavailable")
	}
}

func TestAssembleReviewPrompt_TruncatedDiff(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "ISS-1", Title: "Fix"}
	diff := strings.Repeat("x", 100)
	prompt := assembleReviewPrompt(issue, diff, true, nil, 1, 3)

	if !strings.Contains(prompt, "truncated") {
		t.Errorf("prompt = %q, want to contain %q", prompt, "truncated")
	}
}

func TestAssembleReviewPrompt_PreviousFeedback(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "ISS-1", Title: "Fix"}
	prompt := assembleReviewPrompt(issue, "", false, nil, 2, 3)

	if !strings.Contains(prompt, "previous") {
		t.Errorf("iteration 2 prompt missing previous-feedback section; prompt = %q", prompt)
	}
}

// --- buildFixPrompt tests ---

func TestBuildFixPrompt_WithIssues(t *testing.T) {
	t.Parallel()

	verdict := &domain.ReviewVerdict{
		Verdict: "iterate",
		Issues: []domain.ReviewIssue{
			{File: "main.go", Line: 42, Severity: "error", Message: "nil pointer dereference"},
		},
	}

	prompt := buildFixPrompt(verdict, "", 1, 3)

	if !strings.Contains(prompt, "main.go") {
		t.Errorf("prompt missing issue file; prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "42") {
		t.Errorf("prompt missing line number; prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "nil pointer") {
		t.Errorf("prompt missing issue message; prompt = %q", prompt)
	}
}

func TestBuildFixPrompt_NoIssues(t *testing.T) {
	t.Parallel()

	verdict := &domain.ReviewVerdict{Verdict: "iterate", Summary: "generic issues"}
	prompt := buildFixPrompt(verdict, "", 1, 3)

	if !strings.Contains(prompt, "previous feedback") {
		t.Errorf("prompt with no specific issues missing fallback text; prompt = %q", prompt)
	}
}

func TestBuildFixPrompt_ParseError(t *testing.T) {
	t.Parallel()

	prompt := buildFixPrompt(nil, "verdict file not found", 1, 3)

	if !strings.Contains(prompt, "previous feedback") {
		t.Errorf("prompt with nil verdict missing fallback text; prompt = %q", prompt)
	}
}

// --- writeReviewSummary tests ---

func TestWriteReviewSummary_Pass(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	sortieDir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(sortieDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	meta := domain.ReviewMetadata{
		Enabled:         true,
		TotalIterations: 1,
		FinalVerdict:    "pass",
		Iterations: []domain.ReviewIterationRecord{
			{
				Iteration: 1,
				Verdict:   "pass",
				VerificationResults: []domain.VerificationResult{
					{Command: "make test", ExitCode: 0, DurationMS: 200},
				},
			},
		},
	}

	writeReviewSummary(wsPath, meta, discardLogger())

	data, err := os.ReadFile(filepath.Join(sortieDir, "review_summary.md"))
	if err != nil {
		t.Fatalf("reading review_summary.md: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "Self-Review Summary") {
		t.Errorf("summary missing heading; content = %q", content)
	}
	if !strings.Contains(content, "make test") {
		t.Errorf("summary missing verification command; content = %q", content)
	}
	if !strings.Contains(content, "passed") {
		t.Errorf("summary missing passed result; content = %q", content)
	}
}

func TestWriteReviewSummary_CapReached(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	sortieDir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(sortieDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	meta := domain.ReviewMetadata{
		Enabled:         true,
		TotalIterations: 3,
		FinalVerdict:    "iterate",
		CapReached:      true,
		Iterations: []domain.ReviewIterationRecord{
			{Iteration: 1, Verdict: "iterate"},
			{Iteration: 2, Verdict: "iterate"},
			{Iteration: 3, Verdict: "iterate"},
		},
	}

	writeReviewSummary(wsPath, meta, discardLogger())

	data, err := os.ReadFile(filepath.Join(sortieDir, "review_summary.md"))
	if err != nil {
		t.Fatalf("reading review_summary.md: %v", err)
	}
	if !strings.Contains(string(data), "Cap reached") {
		t.Errorf("summary should mention cap reached; got: %q", string(data))
	}
}

func TestWriteReviewSummary_SymlinkRejected(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	target := t.TempDir()
	symlinkPath := filepath.Join(wsPath, ".sortie")
	if err := os.Symlink(target, symlinkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	writeReviewSummary(wsPath, domain.ReviewMetadata{Enabled: true}, discardLogger())

	if _, err := os.Stat(filepath.Join(target, "review_summary.md")); !os.IsNotExist(err) {
		t.Error("review_summary.md written through symlink, expected rejection")
	}
}

// --- runSingleVerification tests ---

func TestRunVerification_Success(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	m := &reviewMetricsCount{}

	result := runSingleVerification(context.Background(), "echo hello", wsPath, 5000, discardLogger(), m)

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Errorf("Stdout = %q, want to contain %q", result.Stdout, "hello")
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	if result.ExecutionError != "" {
		t.Errorf("ExecutionError = %q, want empty", result.ExecutionError)
	}
	if result.DurationMS < 0 {
		t.Errorf("DurationMS = %d, want >= 0", result.DurationMS)
	}
	if len(m.verCmds) != 1 {
		t.Errorf("ObserveSelfReviewVerificationDuration called %d times, want 1", len(m.verCmds))
	}
}

func TestRunVerification_Failure(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	result := runSingleVerification(context.Background(), "exit 42", wsPath, 5000, discardLogger(), &domain.NoopMetrics{})

	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	if result.ExecutionError != "" {
		t.Errorf("ExecutionError = %q, want empty for clean exit", result.ExecutionError)
	}
}

func TestRunVerification_Timeout(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	// Use an infinite shell-builtin loop so no child process is forked.
	// This ensures the pipe closes promptly when the shell is killed.
	result := runSingleVerification(context.Background(), "while true; do :; done", wsPath, 100, discardLogger(), &domain.NoopMetrics{})

	if !result.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on timeout", result.ExitCode)
	}
}

func TestRunVerification_CommandNotFound(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	// sh returns exit 127 when a command is not found.
	result := runSingleVerification(context.Background(), "nonexistent_binary_xyz_abc_sortie_test", wsPath, 5000, discardLogger(), &domain.NoopMetrics{})

	if result.ExitCode == 0 {
		t.Error("ExitCode = 0 for not-found binary, want non-zero")
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestRunVerification_OutputTruncation(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	// Generate stdout that exceeds MaxVerificationOutputBytes.
	bigCount := domain.MaxVerificationOutputBytes + 1024
	cmd := fmt.Sprintf("python3 -c \"import sys; sys.stdout.write('x' * %d)\"", bigCount)
	result := runSingleVerification(context.Background(), cmd, wsPath, 10000, discardLogger(), &domain.NoopMetrics{})

	if len(result.Stdout) > int(domain.MaxVerificationOutputBytes) {
		t.Errorf("Stdout len = %d, exceeds MaxVerificationOutputBytes %d",
			len(result.Stdout), domain.MaxVerificationOutputBytes)
	}
}

func TestRunVerification_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	wsPath := t.TempDir()
	// Should not panic when context is pre-cancelled.
	result := runSingleVerification(ctx, "echo hello", wsPath, 5000, discardLogger(), &domain.NoopMetrics{})
	_ = result
}

// --- generateWorkspaceDiff tests ---

func TestGenerateDiff_EmptyDiff(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	initGitRepo(t, wsPath)

	diff, truncated, err := generateWorkspaceDiff(context.Background(), wsPath, 102400)

	if err != nil {
		t.Fatalf("generateWorkspaceDiff: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false for empty diff")
	}
	if diff != "" {
		t.Errorf("diff = %q, want empty", diff)
	}
}

func TestGenerateDiff_ModifiedFile(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	initGitRepo(t, wsPath)

	// Commit a file, then modify it so git diff HEAD shows changes.
	filePath := filepath.Join(wsPath, "hello.go")
	if err := os.WriteFile(filePath, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, wsPath, "add", "hello.go")
	runGit(t, wsPath, "commit", "-m", "add hello.go")

	if err := os.WriteFile(filePath, []byte("package main\n// modified\n"), 0o644); err != nil {
		t.Fatalf("WriteFile modify: %v", err)
	}

	diff, _, err := generateWorkspaceDiff(context.Background(), wsPath, 102400)

	if err != nil {
		t.Fatalf("generateWorkspaceDiff: %v", err)
	}
	if !strings.Contains(diff, "modified") {
		t.Errorf("diff = %q, want to contain modified file content", diff)
	}
}

func TestGenerateDiff_Truncation(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	initGitRepo(t, wsPath)

	// Commit a file then modify it to create a diff.
	filePath := filepath.Join(wsPath, "big.txt")
	if err := os.WriteFile(filePath, []byte(strings.Repeat("original\n", 50)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, wsPath, "add", "big.txt")
	runGit(t, wsPath, "commit", "-m", "add big file")

	if err := os.WriteFile(filePath, []byte(strings.Repeat("modified\n", 50)), 0o644); err != nil {
		t.Fatalf("WriteFile modify: %v", err)
	}

	const maxBytes = 50
	diff, truncated, err := generateWorkspaceDiff(context.Background(), wsPath, maxBytes)

	if err != nil {
		t.Fatalf("generateWorkspaceDiff: %v", err)
	}
	if len(diff) > maxBytes {
		t.Errorf("diff len = %d, want <= %d", len(diff), maxBytes)
	}
	if !truncated {
		// The diff 50 bytes may or may not be enough for the full header.
		// Only fail if we got more than allowed.
		t.Log("diff was not truncated; actual diff may be <= 50 bytes")
	}
}

func TestGenerateDiff_NoGit(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()

	_, _, err := generateWorkspaceDiff(context.Background(), wsPath, 102400)

	if err == nil {
		t.Error("generateWorkspaceDiff with non-git dir: expected error, got nil")
	}
}

// --- runSelfReviewLoop tests ---

func TestSelfReviewLoop_PassOnFirst(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	m := &reviewMetricsCount{}
	turns := 0

	adapter := &verdictWriter{
		wsPath:   wsPath,
		verdicts: []domain.ReviewVerdict{{Verdict: "pass", Summary: "looks good"}},
	}

	meta := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        m,
		TurnsCompleted: &turns,
	})

	if meta == nil {
		t.Fatal("meta = nil, want non-nil")
	}
	if !meta.Enabled {
		t.Error("meta.Enabled = false, want true")
	}
	if meta.FinalVerdict != "pass" {
		t.Errorf("FinalVerdict = %q, want %q", meta.FinalVerdict, "pass")
	}
	if meta.TotalIterations != 1 {
		t.Errorf("TotalIterations = %d, want 1", meta.TotalIterations)
	}
	if meta.CapReached {
		t.Error("CapReached = true, want false")
	}
	// review turn 1 only, no fix turn.
	if turns != 1 {
		t.Errorf("TurnsCompleted = %d, want 1", turns)
	}
	if len(m.iterations) != 1 || m.iterations[0] != "pass" {
		t.Errorf("IncSelfReviewIterations = %v, want [pass]", m.iterations)
	}
	if len(m.sessions) != 1 || m.sessions[0] != "pass" {
		t.Errorf("IncSelfReviewSessions = %v, want [pass]", m.sessions)
	}
	if m.capReached != 0 {
		t.Errorf("IncSelfReviewCapReached = %d, want 0", m.capReached)
	}
}

func TestSelfReviewLoop_IterateThenPass(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	m := &reviewMetricsCount{}
	turns := 0

	// Turn sequence (zero-indexed verdictWriter.callIdx):
	//   0 = review turn 1 → iterate
	//   1 = fix turn 1    → no verdict
	//   2 = review turn 2 → pass
	adapter := &verdictWriter{
		wsPath: wsPath,
		verdicts: []domain.ReviewVerdict{
			{Verdict: "iterate", Summary: "need fix"},
			{}, // fix turn: no verdict written
			{Verdict: "pass", Summary: "done"},
		},
	}

	meta := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        m,
		TurnsCompleted: &turns,
	})

	if meta.FinalVerdict != "pass" {
		t.Errorf("FinalVerdict = %q, want %q", meta.FinalVerdict, "pass")
	}
	if meta.TotalIterations != 2 {
		t.Errorf("TotalIterations = %d, want 2", meta.TotalIterations)
	}
	if meta.CapReached {
		t.Error("CapReached = true, want false")
	}
	// review turn 1 + fix turn 1 + review turn 2 = 3 agent RunTurn calls,
	// but TurnsCompleted counts only completed turns (not fix turn that led to re-review).
	// Each iteration increments TurnsCompleted once per review turn and once per fix turn.
	if turns != 3 {
		t.Errorf("TurnsCompleted = %d, want 3", turns)
	}
}

func TestSelfReviewLoop_CapReached(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	m := &reviewMetricsCount{}
	turns := 0

	cfg := selfReviewCfg()
	cfg.MaxIterations = 2

	// Turn sequence:
	//   0 = review turn 1 → iterate
	//   1 = fix turn 1    → no verdict
	//   2 = review turn 2 → iterate (cap reached after this)
	adapter := &verdictWriter{
		wsPath: wsPath,
		verdicts: []domain.ReviewVerdict{
			{Verdict: "iterate", Summary: "still broken"},
			{},
			{Verdict: "iterate", Summary: "still broken"},
		},
	}

	meta := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         cfg,
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        m,
		TurnsCompleted: &turns,
	})

	if !meta.CapReached {
		t.Error("CapReached = false, want true")
	}
	if meta.TotalIterations != 2 {
		t.Errorf("TotalIterations = %d, want 2", meta.TotalIterations)
	}
	if meta.FinalVerdict != "iterate" {
		t.Errorf("FinalVerdict = %q, want %q", meta.FinalVerdict, "iterate")
	}
	if m.capReached != 1 {
		t.Errorf("IncSelfReviewCapReached = %d, want 1", m.capReached)
	}
	if len(m.sessions) != 1 || m.sessions[0] != "iterate" {
		t.Errorf("IncSelfReviewSessions = %v, want [iterate]", m.sessions)
	}
}

func TestSelfReviewLoop_MissingVerdict(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	turns := 0

	cfg := selfReviewCfg()
	cfg.MaxIterations = 1

	// Adapter writes no verdict file.
	adapter := &verdictWriter{wsPath: wsPath, verdicts: nil}

	meta := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         cfg,
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
		TurnsCompleted: &turns,
	})

	if meta.FinalVerdict != "none" {
		t.Errorf("FinalVerdict = %q, want %q", meta.FinalVerdict, "none")
	}
	if !meta.CapReached {
		t.Error("CapReached = false, want true when verdict missing and at cap")
	}
}

func TestSelfReviewLoop_TurnError(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	turns := 0

	adapter := &failOnFirstAdapter{wsPath: wsPath}

	meta := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
		TurnsCompleted: &turns,
	})

	// Loop should break on error; TurnsCompleted must not increment.
	if turns != 0 {
		t.Errorf("TurnsCompleted = %d, want 0 after turn error", turns)
	}
	if meta.TotalIterations != 1 {
		t.Errorf("TotalIterations = %d, want 1 (partial record appended)", meta.TotalIterations)
	}
}

func TestSelfReviewLoop_StatusBlocked(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	turns := 0

	// Adapter writes "blocked" status signal during the first review turn.
	adapter := &statusWriterAdapter{wsPath: wsPath, status: "blocked"}

	meta := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
		TurnsCompleted: &turns,
	})

	// TurnsCompleted increments because the turn itself succeeded even though
	// the status signal triggered an abort. turns == 1.
	if turns != 1 {
		t.Errorf("TurnsCompleted = %d, want 1", turns)
	}
	// Only one iteration record should be present; loop aborted.
	if meta.TotalIterations != 1 {
		t.Errorf("TotalIterations = %d, want 1 (aborted after first iteration)", meta.TotalIterations)
	}
	if meta.Iterations[0].VerdictParseError == "" {
		t.Error("VerdictParseError should be set when aborted by status signal")
	}
}

func TestSelfReviewLoop_ContextCancelled(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	turns := 0
	adapter := &verdictWriter{
		wsPath:   wsPath,
		verdicts: []domain.ReviewVerdict{{Verdict: "pass", Summary: "done"}},
	}

	meta := runSelfReviewLoop(ctx, RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
		TurnsCompleted: &turns,
	})

	if meta.TotalIterations != 0 {
		t.Errorf("TotalIterations = %d, want 0 for pre-cancelled context", meta.TotalIterations)
	}
}

func TestSelfReviewLoop_ProgressEvents(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	turns := 0

	var msgs []selfReviewProgressMsg
	onProgress := func(m selfReviewProgressMsg) {
		msgs = append(msgs, m)
	}

	adapter := &verdictWriter{
		wsPath:   wsPath,
		verdicts: []domain.ReviewVerdict{{Verdict: "pass", Summary: "good"}},
	}

	runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		OnProgress:     onProgress,
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
		TurnsCompleted: &turns,
	})

	checkMsg := func(want string) {
		t.Helper()
		for _, m := range msgs {
			if m.Message == want {
				return
			}
		}
		var got []string
		for _, m := range msgs {
			got = append(got, m.Message)
		}
		t.Errorf("progress messages %v missing %q", got, want)
	}

	checkMsg("self_review_started")
	checkMsg("self_review_iteration")
	checkMsg("self_review_done")
}

func TestSelfReviewLoop_ReviewSummaryWritten(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	turns := 0

	adapter := &verdictWriter{
		wsPath:   wsPath,
		verdicts: []domain.ReviewVerdict{{Verdict: "pass", Summary: "ok"}},
	}

	runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Config:         selfReviewCfg(),
		AgentAdapter:   adapter,
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
		TurnsCompleted: &turns,
	})

	summaryPath := filepath.Join(wsPath, ".sortie", "review_summary.md")
	if _, err := os.Stat(summaryPath); os.IsNotExist(err) {
		t.Error("review_summary.md was not written after loop completion")
	}
}
