package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspace"
	"github.com/sortie-ai/sortie/internal/workspacekit"
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
	Session       domain.Session
	Issue         domain.Issue
	WorkspacePath string
	Config        config.SelfReviewConfig
	AgentAdapter  domain.AgentAdapter
	OnEvent       func(issueID string, event domain.AgentEvent)
	// OnTurnResult, when non-nil, receives the result of each review and
	// fix turn.
	OnTurnResult func(domain.TurnResult)
	OnProgress   func(selfReviewProgressMsg)
	// OnTurnStarted, when non-nil, is called on the worker goroutine
	// before each review and fix turn.
	OnTurnStarted func()

	Logger         *slog.Logger
	Metrics        domain.Metrics
	TurnsCompleted *int

	// TurnTimeoutMS bounds each agent turn the phase runs. Sourced
	// from the attempt-start config snapshot, not re-read per turn.
	TurnTimeoutMS int
}

// maxVerdictFileBytes is the cap on .sortie/review_verdict.json reads.
const maxVerdictFileBytes = 65536

// cappedWriter captures up to max bytes of written data, silently
// discarding excess. Write always reports the full input length so the
// writing subprocess never blocks on a full pipe buffer.
type cappedWriter struct {
	buf strings.Builder
	max int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() < w.max {
		remaining := min(w.max-w.buf.Len(), len(p))
		w.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (w *cappedWriter) String() string {
	return w.buf.String()
}

func generateWorkspaceDiff(ctx context.Context, workspacePath string, maxDiffBytes int) (diff string, originalSize int, truncated bool, err error) {
	// Stage intent-to-add so new files appear in the diff. Best-effort:
	// its outcome is ignored, as before.
	if intentCmd, intentErr := workspace.GitCommand(ctx, workspacePath, "add", "--intent-to-add", "."); intentErr == nil {
		_, _ = procutil.RunCapture(intentCmd, procutil.DefaultStopGrace, procutil.CaptureParams{})
	}

	output, cmdErr := runGitDiffCombined(ctx, workspacePath, "diff", "HEAD")
	if cmdErr != nil {
		// Fallback: try without HEAD (empty repo with staged files).
		output2, err2 := runGitDiffCombined(ctx, workspacePath, "diff")
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

// runGitDiffCombined runs a Git diff command with stdout and stderr
// merged into one sink, matching the combined-output shape the
// self-review prompt has always embedded.
func runGitDiffCombined(ctx context.Context, workspacePath string, args ...string) ([]byte, error) {
	cmd, err := workspace.GitCommand(ctx, workspacePath, args...)
	if err != nil {
		return nil, err
	}
	var combined bytes.Buffer
	result, err := procutil.RunCapture(cmd, procutil.DefaultStopGrace, procutil.CaptureParams{Stdout: &combined, Stderr: &combined})
	if err != nil {
		return nil, err
	}
	if result.WaitErr != nil {
		return combined.Bytes(), result.WaitErr
	}
	// A descendant that outlives git can hold the capture pipe open past
	// the drain bound, leaving combined a prefix of the real diff.
	// Reporting that prefix as the diff would embed silently truncated
	// input in the review prompt with no truncation marker.
	if !result.OutputComplete {
		return combined.Bytes(), fmt.Errorf("git %s: output did not complete within %s", args[0], procutil.DefaultDrainGrace)
	}
	return combined.Bytes(), nil
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

	start := time.Now()
	if verifyErr := workspacekit.VerifyDir(workspacePath); verifyErr != nil {
		logger.Warn("verification command failed to start",
			slog.String("command", command),
			slog.Any("error", verifyErr),
		)
		return domain.VerificationResult{
			Command:        command,
			ExitCode:       -1,
			DurationMS:     time.Since(start).Milliseconds(),
			ExecutionError: verifyErr.Error(),
		}
	}

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command) //nolint:gosec // command comes from operator-controlled config
	cmd.Dir = workspacePath

	// cappedWriter never blocks the writing subprocess, so RunCapture's
	// reader goroutines never stall on a full buffer.
	var stdoutBuf, stderrBuf cappedWriter
	stdoutBuf.max = int(domain.MaxVerificationOutputBytes)
	stderrBuf.max = int(domain.MaxVerificationOutputBytes)

	result, startErr := procutil.RunCapture(cmd, procutil.DefaultStopGrace, procutil.CaptureParams{
		Stdout: &stdoutBuf,
		Stderr: &stderrBuf,
		Logger: logger,
	})
	duration := time.Since(start)

	if startErr != nil {
		logger.Warn("verification command failed to start",
			slog.String("command", command),
			slog.Any("error", startErr),
		)
		return domain.VerificationResult{
			Command:        command,
			ExitCode:       -1,
			DurationMS:     duration.Milliseconds(),
			ExecutionError: startErr.Error(),
		}
	}

	metrics.ObserveSelfReviewVerificationDuration(command, duration.Seconds())

	// RunCapture drains output after the direct child is reaped, so a
	// descendant that outlives the command can carry the context past
	// its deadline once the command itself has already finished. The
	// wait records which of the two happened; the context read here no
	// longer can.
	endedOnItsOwn := !procutil.StoppedByCancellation(result.WaitErr)

	if result.TerminatedLeftovers && endedOnItsOwn {
		logger.Info(procutil.LeftoversTerminatedMessage, slog.String("command", command)) //nolint:sloglint // procutil.LeftoversTerminatedMessage is a fixed string constant
	}

	if !endedOnItsOwn && cmdCtx.Err() == context.DeadlineExceeded {
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
	if result.WaitErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](result.WaitErr); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return domain.VerificationResult{
				Command:        command,
				ExitCode:       -1,
				Stdout:         stdoutBuf.String(),
				Stderr:         stderrBuf.String(),
				DurationMS:     duration.Milliseconds(),
				ExecutionError: result.WaitErr.Error(),
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
	data, err := workspacekit.ReadSortieFile(workspacePath, "review_verdict.json", maxVerdictFileBytes)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, "", "verdict file not found"
		case errors.Is(err, workspacekit.ErrLink),
			errors.Is(err, workspacekit.ErrNotDirectory),
			errors.Is(err, workspacekit.ErrNotPlainFile),
			errors.Is(err, workspacekit.ErrChanged):
			return nil, "", "refusing to read verdict file: " + err.Error()
		case errors.Is(err, workspacekit.ErrTooLarge):
			return nil, "", "verdict file exceeds 64 KB size limit"
		default:
			return nil, "", "verdict read error: " + err.Error()
		}
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
	sb.WriteString("fix the identified issues.\n\n")
	sb.WriteString("4. During this review phase, report your outcome through `.sortie/review_verdict.json`,\n")
	sb.WriteString("not through `.sortie/status`. Writing \"needs-human-review\" to `.sortie/status` here\n")
	sb.WriteString("does not end the phase and does not substitute for a verdict. `blocked` remains\n")
	sb.WriteString("available and still ends the phase — use it when you genuinely cannot carry this\n")
	sb.WriteString("work further.\n")

	return sb.String()
}

func buildFixPrompt(verdict *domain.ReviewVerdict, parseErr string, iteration, maxIterations int) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "## Self-Review Fix: Iteration %d of %d\n\n", iteration, maxIterations)
	sb.WriteString("The self-review identified issues that need to be fixed. Your previous review feedback\n")
	sb.WriteString("is in your conversation history above.\n\n")

	if parseErr != "" {
		sb.WriteString("The orchestrator could not parse your previous review verdict.\n")
		sb.WriteString("Ensure `.sortie/review_verdict.json` is present and valid JSON with a `verdict` field set to `pass` or `iterate`.\n")
		fmt.Fprintf(&sb, "Parse error: %s\n\n", parseErr)
	}

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

	if err := workspacekit.WriteSortieFile(workspacePath, "review_summary.md", []byte(sb.String())); err != nil {
		logger.Warn("review summary write failed", slog.Any("error", err))
	}
}

// readAndConsumeStatusSignal reads the A2O status file and removes it when
// the returned signal is recognized, so a recognized value is never
// observed by a later read at the same site.
//
// The removal is best-effort and never changes the returned signal: the
// caller sees the value that was read, whether or not the file was
// actually removed.
func readAndConsumeStatusSignal(workspacePath string, logger *slog.Logger) workspace.StatusSignal {
	signal := workspace.ReadStatusFile(workspacePath, logger)
	if signal.IsRecognized() {
		workspace.CleanupStatusFile(workspacePath, logger)
	}
	return signal
}

func runSelfReviewLoop(ctx context.Context, params RunSelfReviewParams) (*domain.ReviewMetadata, workspace.StatusSignal, bool, error) {
	maxIter := params.Config.MaxIterations
	iterations := make([]domain.ReviewIterationRecord, 0, maxIter)
	logger := params.Logger
	terminalSignal := workspace.StatusNone
	var expiryErr error

	// cancelledAtEnding is read at each cut point rather than once the loop
	// returns, so a phase that ended on its own is never reclassified by a
	// stop request landing afterward.
	var cancelledAtEnding bool

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
			cancelledAtEnding = true
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

		if removeErr := workspacekit.RemoveSortieFile(params.WorkspacePath, "review_verdict.json"); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			logger.Warn("self-review verdict cleanup failed",
				slog.Int("iteration", i),
				slog.Any("error", removeErr),
			)
		}

		if ctx.Err() != nil {
			cancelledAtEnding = true
			break
		}
		if params.OnTurnStarted != nil {
			params.OnTurnStarted()
		}
		reviewResult, turnErr := runBoundedTurn(ctx, params.AgentAdapter, params.Session, domain.RunTurnParams{
			Prompt: reviewPrompt,
			Issue:  params.Issue,
			OnEvent: func(event domain.AgentEvent) {
				params.OnEvent(params.Issue.ID, event)
			},
		}, params.TurnTimeoutMS, logger, slog.Int("iteration", i), slog.String("review_turn", "review"))
		cutByCancel := ctx.Err() != nil
		if params.OnTurnResult != nil {
			params.OnTurnResult(reviewResult)
		}
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
			var agentErr *domain.AgentError
			if errors.As(turnErr, &agentErr) && agentErr.Kind == domain.ErrTurnTimeout {
				expiryErr = fmt.Errorf("self-review review turn (iteration %d): %w", i, turnErr)
			} else if cutByCancel {
				cancelledAtEnding = true
			}
			break
		}

		*params.TurnsCompleted++

		// Check A2O status for early abort signals.
		statusSignal := readAndConsumeStatusSignal(params.WorkspacePath, logger)
		if statusSignal == workspace.StatusBlocked {
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
			terminalSignal = workspace.StatusBlocked
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

		if ctx.Err() != nil {
			cancelledAtEnding = true
			break
		}
		if params.OnTurnStarted != nil {
			params.OnTurnStarted()
		}
		fixResult, fixErr := runBoundedTurn(ctx, params.AgentAdapter, params.Session, domain.RunTurnParams{
			Prompt: fixPrompt,
			Issue:  params.Issue,
			OnEvent: func(event domain.AgentEvent) {
				params.OnEvent(params.Issue.ID, event)
			},
		}, params.TurnTimeoutMS, logger, slog.Int("iteration", i), slog.String("review_turn", "fix"))
		fixCutByCancel := ctx.Err() != nil
		if params.OnTurnResult != nil {
			params.OnTurnResult(fixResult)
		}
		if fixErr != nil {
			logger.Warn("self-review fix turn failed",
				slog.Int("iteration", i),
				slog.Any("error", fixErr),
			)
			var agentErr *domain.AgentError
			if errors.As(fixErr, &agentErr) && agentErr.Kind == domain.ErrTurnTimeout {
				expiryErr = fmt.Errorf("self-review fix turn (iteration %d): %w", i, fixErr)
				note := fmt.Sprintf("turn timeout: %v", fixErr)
				last := &iterations[len(iterations)-1]
				if last.VerdictParseError == "" {
					last.VerdictParseError = note
				} else {
					last.VerdictParseError += "; " + note
				}
			} else if fixCutByCancel {
				cancelledAtEnding = true
			}
			break
		}

		*params.TurnsCompleted++

		// Check A2O status after fix turn.
		statusSignal = readAndConsumeStatusSignal(params.WorkspacePath, logger)
		if statusSignal == workspace.StatusBlocked {
			logger.Info("self-review aborted by agent status after fix",
				slog.Int("iteration", i),
				slog.String("status", string(statusSignal)),
			)
			iterations[len(iterations)-1].VerdictParseError = fmt.Sprintf("aborted: agent status %q", statusSignal)
			terminalSignal = workspace.StatusBlocked
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

	return meta, terminalSignal, cancelledAtEnding, expiryErr
}
