// Package claude implements [domain.AgentAdapter] for the Claude Code
// CLI. It launches Claude Code as a subprocess in headless mode,
// reads newline-delimited JSON from stdout, and normalizes events into
// domain types. Registered under kind "claude-code" via an init
// function. Safe for concurrent use: each [ClaudeCodeAdapter.RunTurn]
// call operates on an independent subprocess.
package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("claude-code", NewClaudeCodeAdapter, registry.AdapterMeta{
		RequiresCommand: true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*ClaudeCodeAdapter)(nil)

// ClaudeCodeAdapter satisfies [domain.AgentAdapter] by managing Claude
// Code CLI subprocesses. One adapter instance serves all concurrent
// sessions; per-session state is held in [sessionState] via the
// [domain.Session] Internal field.
type ClaudeCodeAdapter struct {
	passthrough passthroughConfig
}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the Claude Code session ID and subprocess
// handle across turns.
type sessionState struct {
	workspacePath   string
	command         string
	claudeSessionID string
	isContinuation  bool
	agentConfig     domain.AgentConfig
	turnCount       int

	// mu guards proc and waitCh for concurrent access from
	// StopSession and gracefulKill.
	mu     sync.Mutex
	proc   *os.Process
	waitCh chan struct{} // closed when cmd.Wait() completes; nil when no process is running
}

// NewClaudeCodeAdapter creates a [ClaudeCodeAdapter] from adapter
// configuration. The config parameter is the raw map from the
// "claude-code" sub-object in WORKFLOW.md. Command resolution is
// deferred to [ClaudeCodeAdapter.StartSession].
func NewClaudeCodeAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt := parsePassthroughConfig(config)
	return &ClaudeCodeAdapter{passthrough: pt}, nil
}

// StartSession validates the workspace path, resolves the claude
// binary, and initializes per-session state. No subprocess is spawned;
// that happens in [ClaudeCodeAdapter.RunTurn].
func (a *ClaudeCodeAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if params.WorkspacePath == "" {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "empty workspace path",
		}
	}

	absPath, err := filepath.Abs(params.WorkspacePath)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "cannot resolve workspace path",
			Err:     err,
		}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path does not exist",
			Err:     err,
		}
	}
	if !info.IsDir() {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path is not a directory",
		}
	}

	command := params.AgentConfig.Command
	if command == "" {
		command = "claude"
	}
	resolvedPath, err := exec.LookPath(command)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("agent command %q not found", command),
			Err:     err,
		}
	}

	isContinuation := false
	sessionUUID := ""
	if params.ResumeSessionID != "" {
		sessionUUID = params.ResumeSessionID
		isContinuation = true
	} else {
		sessionUUID = newUUID()
	}

	state := &sessionState{
		workspacePath:   absPath,
		command:         resolvedPath,
		claudeSessionID: sessionUUID,
		isContinuation:  isContinuation,
		agentConfig:     params.AgentConfig,
	}

	return domain.Session{
		ID:       sessionUUID,
		Internal: state,
	}, nil
}

// RunTurn executes one agent turn by launching a Claude Code
// subprocess and reading JSONL events from stdout. Events are
// delivered synchronously via params.OnEvent. The subprocess is
// managed with graceful shutdown (SIGTERM → SIGKILL) on context
// cancellation rather than the immediate SIGKILL behavior of
// [exec.CommandContext].
func (a *ClaudeCodeAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("claude: OnEvent must be non-nil")
	}

	state := session.Internal.(*sessionState)
	logger := logging.WithSession(slog.Default().With(slog.String("component", "claude-adapter")), state.claudeSessionID)

	args := buildArgs(state, params.Prompt, a.passthrough)

	cmd := exec.Command(state.command, args...) //nolint:gosec // args are constructed programmatically, not from untrusted shell input
	cmd.Dir = state.workspacePath
	cmd.Env = os.Environ()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdout pipe",
			Err:     err,
		}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stderr pipe",
			Err:     err,
		}
	}

	// Lock before Start to prevent a race with StopSession.
	state.mu.Lock()
	err = cmd.Start()
	if err != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to start subprocess",
			Err:     err,
		}
	}
	state.proc = cmd.Process
	state.waitCh = make(chan struct{})
	waitCh := state.waitCh // local copy for cleanup closures
	state.mu.Unlock()

	state.turnCount++

	go drainStderr(stderrPipe, logger)

	// Monitor context cancellation for graceful shutdown.
	doneCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			gracefulKill(state)
		case <-doneCh:
		}
	}()

	// Read and parse stdout line by line.
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lastResult *rawEvent
	var usage domain.TokenUsage

	for scanner.Scan() {
		line := scanner.Bytes()
		now := time.Now().UTC()

		event, parseErr := parseEvent(line)
		if parseErr != nil {
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventMalformed,
				Timestamp: now,
				Message:   truncate(string(line), 500),
			})
			continue
		}

		switch event.Type {
		case "system":
			switch event.Subtype {
			case "init":
				if event.SessionID != "" {
					state.claudeSessionID = event.SessionID
				}
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventSessionStarted,
					Timestamp: now,
					AgentPID:  strconv.Itoa(cmd.Process.Pid),
					SessionID: state.claudeSessionID,
					Message:   "session started",
				})
			case "api_retry":
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventNotification,
					Timestamp: now,
					Message:   formatAPIRetry(event),
				})
			default:
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventNotification,
					Timestamp: now,
					Message:   event.summary(),
				})
			}

		case "assistant":
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   summarizeAssistant(event),
			})

		case "result":
			captured := event
			lastResult = &captured
			usage = normalizeUsage(event.Usage)
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventTokenUsage,
				Timestamp: now,
				Usage:     usage,
			})

		case "stream_event":
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
			})

		default:
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventOtherMessage,
				Timestamp: now,
				Message:   event.summary(),
			})
		}
	}

	// Check scanner error (e.g., buffer overflow).
	if scanErr := scanner.Err(); scanErr != nil {
		gracefulKill(state)
		cmd.Wait() //nolint:errcheck // best-effort reap; exit code is irrelevant on scanner failure
		close(waitCh)
		close(doneCh)
		state.mu.Lock()
		state.proc = nil
		state.waitCh = nil
		state.mu.Unlock()
		now := time.Now().UTC()
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   "stdout read error: " + scanErr.Error(),
		})
		return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "stdout scanner error",
				Err:     scanErr,
			}
	}

	// Wait for process to exit. cmd.Wait is the sole waiter;
	// StopSession and gracefulKill only signal and wait on waitCh.
	waitErr := cmd.Wait()
	close(waitCh)
	close(doneCh)

	// Clear subprocess reference.
	state.mu.Lock()
	state.proc = nil
	state.waitCh = nil
	state.mu.Unlock()

	now := time.Now().UTC()

	// Determine exit reason.
	if ctx.Err() != nil {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnCancelled,
			Timestamp: now,
			Message:   "context cancelled",
		})
		return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnCancelled,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnCancelled,
				Message: "turn cancelled",
				Err:     ctx.Err(),
			}
	}

	exitCode := extractExitCode(waitErr)

	if exitCode == 127 {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   "claude binary not found",
		})
		return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "exit code 127",
			}
	}

	if wasSignaled(waitErr) {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnCancelled,
			Timestamp: now,
			Message:   "killed by signal",
		})
		return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnCancelled,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnCancelled,
				Message: "killed by signal",
			}
	}

	if lastResult != nil {
		if lastResult.Subtype == "success" && !lastResult.IsError {
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventTurnCompleted,
				Timestamp: now,
				Message:   truncate(lastResult.Result, 500),
			})
			return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnCompleted,
				Usage:      usage,
			}, nil
		}
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   lastResult.Subtype,
		})
		return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: lastResult.Subtype,
			}
	}

	if exitCode != 0 {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   "non-zero exit",
		})
		return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: fmt.Sprintf("exit code %d", exitCode),
			}
	}

	// No result event and exit code 0 — treat as success.
	params.OnEvent(domain.AgentEvent{
		Type:      domain.EventTurnCompleted,
		Timestamp: now,
	})
	return domain.TurnResult{
		SessionID:  state.claudeSessionID,
		ExitReason: domain.EventTurnCompleted,
		Usage:      usage,
	}, nil
}

// StopSession terminates a running Claude Code subprocess gracefully.
// Sends SIGTERM, waits up to 5 seconds, then sends SIGKILL. Safe to
// call when no subprocess is running.
func (a *ClaudeCodeAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state := session.Internal.(*sessionState)

	state.mu.Lock()
	proc := state.proc
	state.proc = nil
	waitCh := state.waitCh
	state.mu.Unlock()

	if proc == nil {
		return nil
	}

	// Send SIGTERM for graceful shutdown.
	_ = proc.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort signal; process may already be dead

	select {
	case <-waitCh:
		return nil
	case <-time.After(5 * time.Second):
		_ = proc.Kill() //nolint:errcheck // best-effort kill
		return nil
	case <-ctx.Done():
		_ = proc.Kill() //nolint:errcheck // best-effort kill
		return ctx.Err()
	}
}

// EventStream returns nil. The Claude Code adapter delivers events
// synchronously via the [domain.RunTurnParams] OnEvent callback.
func (a *ClaudeCodeAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}

// gracefulKill sends SIGTERM and schedules a SIGKILL escalation after
// 5 seconds. It returns immediately; the caller (RunTurn) is
// responsible for calling cmd.Wait. Safe to call when proc is nil.
func gracefulKill(state *sessionState) {
	state.mu.Lock()
	proc := state.proc
	state.mu.Unlock()

	if proc == nil {
		return
	}

	_ = proc.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort signal

	// Schedule SIGKILL escalation. The timer checks state.proc
	// under the lock; once RunTurn clears proc the kill is skipped.
	time.AfterFunc(5*time.Second, func() {
		state.mu.Lock()
		p := state.proc
		state.mu.Unlock()
		if p != nil {
			_ = p.Kill() //nolint:errcheck // best-effort kill
		}
	})
}

// drainStderr reads stderr from the subprocess line by line and logs
// each line at debug level. Returns when the pipe closes.
func drainStderr(r io.Reader, logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logger.Debug("agent stderr", slog.String("line", scanner.Text()))
	}
}

// extractExitCode returns the process exit code from an
// [*exec.ExitError], or -1 if the error is not an ExitError or is
// nil.
func extractExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// wasSignaled returns true if the process was terminated by a signal.
func wasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}
