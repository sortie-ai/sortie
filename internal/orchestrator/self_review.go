package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// selfReviewProgressMsg carries self-review loop progress from the
// worker goroutine to the orchestrator event loop.
type selfReviewProgressMsg struct {
	IssueID       string
	Message       string
	Iteration     int
	MaxIterations int
}

// RunSelfReviewParams captures all inputs for runSelfReviewLoop.
type RunSelfReviewParams struct {
	Session        domain.Session
	Issue          domain.Issue
	WorkspacePath  string
	Config         config.SelfReviewConfig
	AgentAdapter   domain.AgentAdapter
	OnEvent        func(issueID string, event domain.AgentEvent)
	OnProgress     func(selfReviewProgressMsg)
	Logger         *slog.Logger
	Metrics        domain.Metrics
	TurnsCompleted *int
}

// maxVerdictFileBytes is the cap on .sortie/review_verdict.json reads.
const maxVerdictFileBytes = 65536

func generateWorkspaceDiff(ctx context.Context, workspacePath string, maxDiffBytes int) (diff string, originalSize int, truncated bool, err error) {
	// Stage intent-to-add so new files appear in the diff.
	intentCmd := exec.CommandContext(ctx, "git", "add", "--intent-to-add", ".")
	intentCmd.Dir = workspacePath
	_ = intentCmd.Run() // best-effort

	cmd := exec.CommandContext(ctx, "git", "diff", "HEAD")
	cmd.Dir = workspacePath
	output, cmdErr := cmd.CombinedOutput()
	if cmdErr != nil {
		// Fallback: try without HEAD (empty repo with staged files).
		cmd2 := exec.CommandContext(ctx, "git", "diff")
		cmd2.Dir = workspacePath
		output2, err2 := cmd2.CombinedOutput()
		if err2 != nil {
			return "", 0, false, fmt.Errorf("git diff failed: %w (fallback: %w)", cmdErr, err2)
		}
		output = output2
	}

	originalSize = len(output)
	if maxDiffBytes > 0 && len(output) > maxDiffBytes {
		output = output[:maxDiffBytes]
		truncated = true
	}

	return string(output), originalSize, truncated, nil
}

func runSingleVerification(ctx context.Context, command, workspacePath string, timeoutMS int, logger *slog.Logger, metrics domain.Metrics) domain.VerificationResult {
	var cmdCtx context.Context
	var cancel context.CancelFunc
	if timeoutMS > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	} else {
		cmdCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command) //nolint:gosec // command comes from operator-controlled config
	cmd.Dir = workspacePath
	procutil.SetProcessGroup(cmd)

	var stdoutBuf, stderrBuf strings.Builder

	stdoutPipe, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return domain.VerificationResult{
			Command:        command,
			ExitCode:       -1,
			ExecutionError: pipeErr.Error(),
		}
	}
	stderrPipe, pipeErr := cmd.StderrPipe()
	if pipeErr != nil {
		return domain.VerificationResult{
			Command:        command,
			ExitCode:       -1,
			ExecutionError: pipeErr.Error(),
		}
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		duration := time.Since(start)
		logger.Warn("verification command failed to start",
			slog.String("command", command),
			slog.Any("error", err),
		)
		return domain.VerificationResult{
			Command:        command,
			ExitCode:       -1,
			DurationMS:     duration.Milliseconds(),
			ExecutionError: err.Error(),
		}
	}

	// Drain stdout and stderr concurrently to avoid deadlock when either
	// pipe fills its OS buffer while the other is not being read.
	var wg sync.WaitGroup
	wg.Add(2)
	readLimited := func(r io.Reader, w *strings.Builder) {
		defer wg.Done()
		limited := io.LimitReader(r, domain.MaxVerificationOutputBytes)
		_, _ = io.Copy(w, limited)
	}
	go readLimited(stdoutPipe, &stdoutBuf)
	go readLimited(stderrPipe, &stderrBuf)

	// On some platforms (notably Windows with MSYS2 shells), the write
	// end of the stdout/stderr pipes may not close promptly when the
	// process is killed via context cancellation. Explicitly close the
	// read ends when the context expires so that the drain goroutines
	// are unblocked and wg.Wait() can return.
	drainDone := make(chan struct{})
	go func() {
		select {
		case <-cmdCtx.Done():
			stdoutPipe.Close()
			stderrPipe.Close()
		case <-drainDone:
		}
	}()
	wg.Wait()
	close(drainDone)

	err := cmd.Wait()
	duration := time.Since(start)

	metrics.ObserveSelfReviewVerificationDuration(command, duration.Seconds())

	if cmdCtx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			_ = procutil.KillProcessGroup(cmd.Process.Pid)
		}
		logger.Info("verification command timed out",
			slog.String("command", command),
			slog.Int64("duration_ms", duration.Milliseconds()),
		)
		return domain.VerificationResult{
			Command:    command,
			ExitCode:   -1,
			Stdout:     stdoutBuf.String(),
			Stderr:     stderrBuf.String(),
			DurationMS: duration.Milliseconds(),
			TimedOut:   true,
		}
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return domain.VerificationResult{
				Command:        command,
				ExitCode:       -1,
				Stdout:         stdoutBuf.String(),
				Stderr:         stderrBuf.String(),
				DurationMS:     duration.Milliseconds(),
				ExecutionError: err.Error(),
			}
		}
	}

	logger.Debug("verification command completed",
		slog.String("command", command),
		slog.Int("exit_code", exitCode),
		slog.Int64("duration_ms", duration.Milliseconds()),
	)

	return domain.VerificationResult{
		Command:    command,
		ExitCode:   exitCode,
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		DurationMS: duration.Milliseconds(),
	}
}

func readReviewVerdict(workspacePath string) (*domain.ReviewVerdict, string, string) {
	dir := filepath.Join(workspacePath, ".sortie")
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, "", "verdict directory not found"
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, "", "refusing to read from symlinked .sortie"
	}

	path := filepath.Join(dir, "review_verdict.json")

	// Reject symlinked verdict files to prevent reading arbitrary host files.
	fileFI, fileErr := os.Lstat(path)
	if fileErr != nil {
		if os.IsNotExist(fileErr) {
			return nil, "", "verdict file not found"
		}
		return nil, "", fmt.Sprintf("verdict stat error: %v", fileErr)
	}
	if fileFI.Mode()&os.ModeSymlink != 0 {
		return nil, "", "refusing to read symlinked verdict file"
	}
	if !fileFI.Mode().IsRegular() {
		return nil, "", "verdict path is not a regular file"
	}

	f, err := os.Open(path) //nolint:gosec // path is constructed from operator-controlled workspace root
	if err != nil {
		return nil, "", fmt.Sprintf("verdict open error: %v", err)
	}
	defer func() { _ = f.Close() }()

	limited := io.LimitReader(f, maxVerdictFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Sprintf("verdict read error: %v", err)
	}
	if len(data) > maxVerdictFileBytes {
		return nil, "", "verdict file exceeds 64 KB size limit"
	}

	var verdict domain.ReviewVerdict
	if err := json.Unmarshal(data, &verdict); err != nil {
		return nil, string(data), fmt.Sprintf("verdict parse error: %v", err)
	}

	verdict.Verdict = strings.ToLower(strings.TrimSpace(verdict.Verdict))
	if verdict.Verdict != "pass" && verdict.Verdict != "iterate" {
		return nil, string(data), fmt.Sprintf("unrecognized verdict: %q", verdict.Verdict)
	}

	return &verdict, string(data), ""
}

func assembleReviewPrompt(issue domain.Issue, diff string, truncated bool, vresults []domain.VerificationResult, iteration, maxIterations int) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "## Self-Review: Iteration %d of %d\n\n", iteration, maxIterations)
	sb.WriteString("You are reviewing your own changes. Analyze the diff and verification results below, then\n")
	sb.WriteString("write your verdict to `.sortie/review_verdict.json`.\n\n")

	sb.WriteString("### Original Task\n\n")
	sb.WriteString(issue.Title)
	sb.WriteString("\n")
	if issue.Description != "" {
		sb.WriteString(issue.Description)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString("### Workspace Diff\n\n")
	if diff == "" {
		sb.WriteString("[Diff unavailable]\n\n")
	} else {
		if truncated {
			fmt.Fprintf(&sb, "[Diff truncated at %d bytes]\n\n", len(diff))
		}
		sb.WriteString("```diff\n")
		sb.WriteString(diff)
		sb.WriteString("\n```\n\n")
	}

	sb.WriteString("### Verification Results\n\n")
	for _, result := range vresults {
		fmt.Fprintf(&sb, "#### Command: `%s`\n", result.Command)
		fmt.Fprintf(&sb, "- Exit code: %d\n", result.ExitCode)
		fmt.Fprintf(&sb, "- Duration: %dms\n", result.DurationMS)
		if result.TimedOut {
			sb.WriteString("- **TIMED OUT**\n")
		}
		if result.ExecutionError != "" {
			fmt.Fprintf(&sb, "- **EXECUTION ERROR:** %s\n", result.ExecutionError)
		}
		sb.WriteString("\n")

		if result.Stdout != "" {
			sb.WriteString("**stdout:**\n```\n")
			sb.WriteString(result.Stdout)
			sb.WriteString("\n```\n\n")
		}

		if result.Stderr != "" {
			sb.WriteString("**stderr:**\n```\n")
			sb.WriteString(result.Stderr)
			sb.WriteString("\n```\n\n")
		}
	}

	if iteration > 1 {
		sb.WriteString("### Previous Review Feedback\n\n")
		fmt.Fprintf(&sb, "Your previous review found issues. This is iteration %d. Focus on the issues identified in the previous review.\n\n", iteration)
	}

	sb.WriteString("### Instructions\n\n")
	sb.WriteString("1. Review the diff for correctness, style, and completeness relative to the original task.\n")
	sb.WriteString("2. Check whether the verification commands passed. If any failed, identify the root cause.\n")
	sb.WriteString("3. Write your verdict as a JSON file:\n\n")
	sb.WriteString("Create or overwrite the file `.sortie/review_verdict.json` with the following JSON content:\n\n")
	sb.WriteString("```json\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"verdict\": \"pass or iterate\",\n")
	sb.WriteString("  \"summary\": \"one-line summary\",\n")
	sb.WriteString("  \"issues\": [\n")
	sb.WriteString("    {\n")
	sb.WriteString("      \"file\": \"path/to/file.go\",\n")
	sb.WriteString("      \"line\": 42,\n")
	sb.WriteString("      \"severity\": \"error\",\n")
	sb.WriteString("      \"message\": \"description and fix direction\"\n")
	sb.WriteString("    }\n")
	sb.WriteString("  ]\n")
	sb.WriteString("}\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Use \"pass\" when the changes are correct and verification passes. Use \"iterate\" when\n")
	sb.WriteString("issues need to be fixed. On \"iterate\", the orchestrator will give you another turn to\n")
	sb.WriteString("fix the identified issues.\n")

	return sb.String()
}

func buildFixPrompt(verdict *domain.ReviewVerdict, _ string, iteration, maxIterations int) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "## Self-Review Fix: Iteration %d of %d\n\n", iteration, maxIterations)
	sb.WriteString("The self-review identified issues that need to be fixed. Your previous review feedback\n")
	sb.WriteString("is in your conversation history above.\n\n")

	if verdict != nil && len(verdict.Issues) > 0 {
		sb.WriteString("Focus on these issues:\n")
		for _, issue := range verdict.Issues {
			if issue.Line > 0 {
				fmt.Fprintf(&sb, "- [%s] %s:%d — %s\n", issue.Severity, issue.File, issue.Line, issue.Message)
			} else {
				fmt.Fprintf(&sb, "- [%s] %s — %s\n", issue.Severity, issue.File, issue.Message)
			}
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("Review your previous feedback and fix the issues you identified.\n\n")
	}

	sb.WriteString("After making fixes, the orchestrator will run verification commands and review again.\n")

	return sb.String()
}

func writeReviewSummary(workspacePath string, meta domain.ReviewMetadata, logger *slog.Logger) {
	dir := filepath.Join(workspacePath, ".sortie")

	fi, err := os.Lstat(dir)
	if err != nil {
		logger.Warn("review summary: cannot stat .sortie directory", slog.Any("error", err))
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		logger.Warn("review summary: .sortie is a symlink, refusing to write")
		return
	}

	var sb strings.Builder
	sb.WriteString("## Self-Review Summary\n\n")

	status := meta.FinalVerdict
	if meta.CapReached {
		status = fmt.Sprintf("Cap reached (iteration %d of %d)", meta.TotalIterations, len(meta.Iterations))
	} else if status == "pass" {
		status = fmt.Sprintf("Passed (iteration %d)", meta.TotalIterations)
	}

	fmt.Fprintf(&sb, "- **Status:** %s\n", status)

	passedCount := 0
	failedCount := 0
	if len(meta.Iterations) > 0 {
		last := meta.Iterations[len(meta.Iterations)-1]
		for _, vr := range last.VerificationResults {
			if vr.ExitCode == 0 {
				passedCount++
			} else {
				failedCount++
			}
		}
	}
	fmt.Fprintf(&sb, "- **Verification commands:** %d passed, %d failed\n", passedCount, failedCount)
	fmt.Fprintf(&sb, "- **Final verdict:** %s\n\n", meta.FinalVerdict)

	for _, iter := range meta.Iterations {
		fmt.Fprintf(&sb, "### Iteration %d\n", iter.Iteration)
		if iter.Verdict != "" {
			fmt.Fprintf(&sb, "- Verdict: %s\n", iter.Verdict)
		} else if iter.VerdictParseError != "" {
			fmt.Fprintf(&sb, "- Verdict: (error: %s)\n", iter.VerdictParseError)
		}

		for _, vr := range iter.VerificationResults {
			result := "passed"
			if vr.ExitCode != 0 {
				result = fmt.Sprintf("failed (exit %d)", vr.ExitCode)
			}
			if vr.TimedOut {
				result = "timed out"
			}
			fmt.Fprintf(&sb, "- Verification: `%s` %s\n", vr.Command, result)
		}
		sb.WriteString("\n")
	}

	tmpPath := filepath.Join(dir, "review_summary.md.tmp")
	outPath := filepath.Join(dir, "review_summary.md")
	if err := os.WriteFile(tmpPath, []byte(sb.String()), 0o600); err != nil {
		logger.Warn("review summary write failed", slog.Any("error", err))
		return
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		logger.Warn("review summary rename failed", slog.Any("error", err))
	}
}

func runSelfReviewLoop(ctx context.Context, params RunSelfReviewParams) *domain.ReviewMetadata {
	maxIter := params.Config.MaxIterations
	iterations := make([]domain.ReviewIterationRecord, 0, maxIter)
	logger := params.Logger

	if params.OnProgress != nil {
		params.OnProgress(selfReviewProgressMsg{
			IssueID:       params.Issue.ID,
			Message:       "self_review_started",
			Iteration:     0,
			MaxIterations: maxIter,
		})
	}

	for i := 1; i <= maxIter; i++ {
		if ctx.Err() != nil {
			break
		}

		diff, diffSize, truncated, diffErr := generateWorkspaceDiff(ctx, params.WorkspacePath, params.Config.MaxDiffBytes)
		if diffErr != nil {
			logger.Warn("self-review diff generation failed", slog.Any("error", diffErr))
			diff = ""
			diffSize = 0
		}

		var verificationResults []domain.VerificationResult
		for _, cmd := range params.Config.VerificationCommands {
			if ctx.Err() != nil {
				break
			}
			result := runSingleVerification(ctx, cmd, params.WorkspacePath, params.Config.VerificationTimeoutMS, logger, params.Metrics)
			verificationResults = append(verificationResults, result)
		}

		reviewPrompt := assembleReviewPrompt(params.Issue, diff, truncated, verificationResults, i, maxIter)

		// Remove previous verdict file only when .sortie is a real directory.
		sortieDirPath := filepath.Join(params.WorkspacePath, ".sortie")
		sortieInfo, sortieErr := os.Lstat(sortieDirPath)
		if sortieErr == nil {
			if sortieInfo.Mode()&os.ModeSymlink == 0 && sortieInfo.IsDir() {
				_ = os.Remove(filepath.Join(sortieDirPath, "review_verdict.json"))
			} else {
				logger.Warn("self-review verdict cleanup skipped: .sortie is a symlink or not a directory",
					slog.Int("iteration", i),
				)
			}
		} else if !os.IsNotExist(sortieErr) {
			logger.Warn("self-review verdict cleanup failed",
				slog.Int("iteration", i),
				slog.Any("error", sortieErr),
			)
		}

		_, turnErr := params.AgentAdapter.RunTurn(ctx, params.Session, domain.RunTurnParams{
			Prompt: reviewPrompt,
			Issue:  params.Issue,
			OnEvent: func(event domain.AgentEvent) {
				params.OnEvent(params.Issue.ID, event)
			},
		})
		if turnErr != nil {
			logger.Warn("self-review turn failed",
				slog.Int("iteration", i),
				slog.Any("error", turnErr),
			)
			iterations = append(iterations, domain.ReviewIterationRecord{
				Iteration:           i,
				DiffSizeBytes:       diffSize,
				DiffTruncated:       truncated,
				VerificationResults: verificationResults,
				VerdictParseError:   fmt.Sprintf("turn error: %v", turnErr),
			})
			break
		}

		*params.TurnsCompleted++

		// Check A2O status for early abort signals.
		statusSignal := workspace.ReadStatusFile(params.WorkspacePath, logger)
		if statusSignal.IsRecognized() {
			logger.Info("self-review aborted by agent status",
				slog.Int("iteration", i),
				slog.String("status", string(statusSignal)),
			)
			iterations = append(iterations, domain.ReviewIterationRecord{
				Iteration:           i,
				DiffSizeBytes:       diffSize,
				DiffTruncated:       truncated,
				VerificationResults: verificationResults,
				VerdictParseError:   fmt.Sprintf("aborted: agent status %q", statusSignal),
			})
			break
		}

		verdict, rawJSON, parseErr := readReviewVerdict(params.WorkspacePath)

		record := domain.ReviewIterationRecord{
			Iteration:           i,
			DiffSizeBytes:       diffSize,
			DiffTruncated:       truncated,
			VerificationResults: verificationResults,
		}
		if verdict != nil {
			record.Verdict = verdict.Verdict
			record.VerdictRaw = rawJSON
		}
		if parseErr != "" {
			record.VerdictParseError = parseErr
		}
		iterations = append(iterations, record)

		verdictLabel := record.Verdict
		if verdictLabel == "" {
			verdictLabel = "none"
		}
		params.Metrics.IncSelfReviewIterations(verdictLabel)

		if params.OnProgress != nil {
			params.OnProgress(selfReviewProgressMsg{
				IssueID:       params.Issue.ID,
				Message:       "self_review_iteration",
				Iteration:     i,
				MaxIterations: maxIter,
			})
		}

		if verdict != nil && verdict.Verdict == "pass" {
			logger.Info("self-review passed", slog.Int("iteration", i))
			break
		}

		if i == maxIter {
			logger.Warn("self-review cap reached",
				slog.Int("iterations", maxIter),
				slog.String("final_verdict", record.Verdict),
			)
			params.Metrics.IncSelfReviewCapReached()
			break
		}

		// "iterate" or missing verdict: give the agent a fix turn.
		if verdict != nil && verdict.Verdict == "iterate" {
			logger.Info("self-review iterate",
				slog.Int("iteration", i),
				slog.String("summary", verdict.Summary),
			)
		} else {
			logger.Warn("self-review verdict missing or invalid",
				slog.Int("iteration", i),
				slog.String("parse_error", parseErr),
			)
		}

		fixPrompt := buildFixPrompt(verdict, parseErr, i, maxIter)

		_, fixErr := params.AgentAdapter.RunTurn(ctx, params.Session, domain.RunTurnParams{
			Prompt: fixPrompt,
			Issue:  params.Issue,
			OnEvent: func(event domain.AgentEvent) {
				params.OnEvent(params.Issue.ID, event)
			},
		})
		if fixErr != nil {
			logger.Warn("self-review fix turn failed",
				slog.Int("iteration", i),
				slog.Any("error", fixErr),
			)
			break
		}

		*params.TurnsCompleted++

		// Check A2O status after fix turn.
		statusSignal = workspace.ReadStatusFile(params.WorkspacePath, logger)
		if statusSignal.IsRecognized() {
			logger.Info("self-review aborted by agent status after fix",
				slog.Int("iteration", i),
				slog.String("status", string(statusSignal)),
			)
			break
		}
	}

	finalVerdict := "none"
	if len(iterations) > 0 {
		last := iterations[len(iterations)-1]
		if last.Verdict != "" {
			finalVerdict = last.Verdict
		}
	}

	capReached := len(iterations) == maxIter && finalVerdict != "pass"

	meta := &domain.ReviewMetadata{
		Enabled:         true,
		Iterations:      iterations,
		TotalIterations: len(iterations),
		FinalVerdict:    finalVerdict,
		CapReached:      capReached,
	}

	writeReviewSummary(params.WorkspacePath, *meta, logger)

	if params.OnProgress != nil {
		params.OnProgress(selfReviewProgressMsg{
			IssueID:       params.Issue.ID,
			Message:       "self_review_done",
			Iteration:     len(iterations),
			MaxIterations: maxIter,
		})
	}

	params.Metrics.IncSelfReviewSessions(finalVerdict)

	return meta
}
