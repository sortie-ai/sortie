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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
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

// ansiEscapeRE matches VT100/ANSI CSI SGR escape sequences emitted by CLI
// tools for color and formatting (e.g. \x1b[31m, \x1b[0m). Compiled once at
// program startup; applied inside stripClaudeMarkup.
var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// inFlightTool tracks a tool_use block that has been seen but whose
// corresponding tool_result has not yet arrived.
type inFlightTool struct {
	Name      string
	Timestamp time.Time
}

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

	// sshHost is the SSH destination for remote execution. Empty for
	// local mode.
	sshHost string

	// remoteCommand is the agent command to run on the remote host
	// when sshHost is non-empty. Empty for local mode. The local
	// command field holds the resolved path to the ssh binary.
	remoteCommand string

	// mu guards proc and waitCh for concurrent access from
	// StopSession and the cmd.Cancel callback.
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

	var resolvedPath string
	var sshHost string
	var remoteCommand string

	if params.SSHHost != "" {
		// SSH mode: resolve "ssh" locally, skip local LookPath for
		// the agent command (it resolves on the remote host).
		sshPath, lookErr := exec.LookPath("ssh")
		if lookErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "ssh binary not found on orchestrator host",
				Err:     lookErr,
			}
		}
		resolvedPath = sshPath
		sshHost = params.SSHHost
		remoteCommand = command
	} else {
		var lookErr error
		resolvedPath, lookErr = exec.LookPath(command)
		if lookErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: fmt.Sprintf("agent command %q not found", command),
				Err:     lookErr,
			}
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
		sshHost:         sshHost,
		remoteCommand:   remoteCommand,
	}

	return domain.Session{
		ID:       sessionUUID,
		Internal: state,
	}, nil
}

// RunTurn executes one agent turn by launching a Claude Code subprocess
// and reading JSONL events from stdout. Events are delivered
// synchronously via params.OnEvent.
//
// Subprocess lifecycle uses [exec.CommandContext] with [exec.Cmd].Cancel
// set to send [syscall.SIGTERM] and [exec.Cmd].WaitDelay set to 5
// seconds. This preserves the SIGTERM-first invariant: on context
// cancellation the agent receives SIGTERM and has 5 seconds to flush
// output before SIGKILL is sent automatically.
func (a *ClaudeCodeAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("claude: OnEvent must be non-nil")
	}

	state := session.Internal.(*sessionState)
	logger := logging.WithSession(slog.Default().With(slog.String("component", "claude-adapter")), state.claudeSessionID)

	args := buildArgs(state, params.Prompt, a.passthrough)

	cmdCtx, cancelCmd := context.WithCancel(ctx)
	defer cancelCmd()

	var cmd *exec.Cmd
	if state.sshHost != "" {
		sshArgs := sshutil.BuildSSHArgs(state.sshHost, state.workspacePath, state.remoteCommand, args, sshutil.SSHOptions{})
		cmd = exec.CommandContext(cmdCtx, state.command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(cmdCtx, state.command, args...) //nolint:gosec // args are constructed programmatically
	}
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second
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
		if ctx.Err() != nil {
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventTurnCancelled,
				Timestamp: time.Now().UTC(),
				Message:   "context cancelled",
			})
			return domain.TurnResult{
					SessionID:  state.claudeSessionID,
					ExitReason: domain.EventTurnCancelled,
				}, &domain.AgentError{
					Kind:    domain.ErrTurnCancelled,
					Message: "turn cancelled",
					Err:     ctx.Err(),
				}
		}
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

	go procutil.DrainStderr(stderrPipe, logger)

	// Read and parse stdout line by line.
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lastResult *rawEvent
	var usage domain.TokenUsage
	inFlight := make(map[string]inFlightTool)

	// Cumulative accumulators for per-assistant-message token usage.
	// Claude Code assistant message usage fields are per-request (not
	// cumulative). The orchestrator delta algorithm requires cumulative
	// values, so we accumulate here and emit cumulative totals.
	var (
		cumulativeInput     int64
		cumulativeOutput    int64
		cumulativeCacheRead int64
		lastModel           string
		emittedUsage        bool
		apiCallStart        time.Time // monotonic timestamp of the last event before an API call
		emittedAPITiming    bool      // true once per-request APIDurationMS has been emitted
	)

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
				// The first API call is imminent. Start the monotonic timer.
				// This first measurement includes agent initialization overhead
				// (workspace scanning, .claude.json parsing, system prompt
				// assembly) that occurs before the actual HTTP request.
				// Subsequent measurements (after user events) do not have this
				// overhead.
				apiCallStart = time.Now()
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
			// Extract model and per-request usage from assistant message.
			if len(event.Message) > 0 {
				var meta rawAssistantMessageMeta
				if err := json.Unmarshal(event.Message, &meta); err == nil {
					if meta.Model != "" {
						lastModel = meta.Model
					}
					if meta.Usage != nil {
						cumulativeInput += meta.Usage.InputTokens
						cumulativeOutput += meta.Usage.OutputTokens
						cumulativeCacheRead += meta.Usage.CacheReadInputTokens
						cumulativeTotal := cumulativeInput + cumulativeOutput
						usage = domain.TokenUsage{
							InputTokens:     cumulativeInput,
							OutputTokens:    cumulativeOutput,
							TotalTokens:     cumulativeTotal,
							CacheReadTokens: cumulativeCacheRead,
						}
						tokenEvt := domain.AgentEvent{
							Type:      domain.EventTokenUsage,
							Timestamp: now,
							Usage:     usage,
							Model:     lastModel,
						}
						if !apiCallStart.IsZero() {
							dur := time.Since(apiCallStart).Milliseconds()
							if dur <= 0 {
								dur = 1 // clamp so the orchestrator accumulates this measurement
							}
							tokenEvt.APIDurationMS = dur
							apiCallStart = time.Time{}
							emittedAPITiming = true
						}
						params.OnEvent(tokenEvt)
						emittedUsage = true
					}
				}
			}
			// Use a monotonic timestamp for in-flight duration math.
			// The wall-clock `now` (from .UTC()) has its monotonic
			// reading stripped, so Sub() would depend on wall time and
			// could go negative on clock adjustment. A separate
			// time.Now() retains the monotonic component.
			observed := time.Now()
			processToolBlocks(event.contentBlocks(), inFlight, observed, now, params.OnEvent)
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   summarizeAssistant(event),
			})

		case "user":
			// Claude Code emits tool results as user-role messages.
			// Correlate with the inFlight map populated from
			// assistant tool_use blocks.
			processToolBlocks(event.contentBlocks(), inFlight, time.Now(), now, params.OnEvent)
			apiCallStart = time.Now() // next API call is imminent

		case "result":
			captured := event
			lastResult = &captured
			usage = normalizeUsage(event.Usage)
			// Only emit token_usage from the result event when no
			// per-assistant-message usage was already emitted. This
			// avoids inflating APIRequestCount in the orchestrator.
			if !emittedUsage {
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventTokenUsage,
					Timestamp: now,
					Usage:     usage,
					Model:     lastModel,
				})
			}

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
		cancelCmd()
		cmd.Wait() //nolint:errcheck,gosec // best-effort reap; exit code is irrelevant on scanner failure
		close(waitCh)
		state.mu.Lock()
		state.proc = nil
		state.waitCh = nil
		state.mu.Unlock()

		// Context cancellation propagates through exec.CommandContext
		// and can surface as a pipe read error. Treat as cancellation.
		if ctx.Err() != nil {
			now := time.Now().UTC()
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
	// StopSession only signals and waits on waitCh.
	waitErr := cmd.Wait()
	close(waitCh)

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

	exitCode := procutil.ExtractExitCode(waitErr)

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

	if procutil.WasSignaled(waitErr) {
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
		// Use turn-level duration_api_ms only when no per-request
		// API timing was emitted, to avoid double-counting.
		var turnAPIDuration int64
		if !emittedAPITiming {
			turnAPIDuration = lastResult.DurationAPI
		}
		if lastResult.Subtype == "success" && !lastResult.IsError {
			params.OnEvent(domain.AgentEvent{
				Type:          domain.EventTurnCompleted,
				Timestamp:     now,
				Message:       truncate(lastResult.Result, 500),
				APIDurationMS: turnAPIDuration,
			})
			return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnCompleted,
				Usage:      usage,
			}, nil
		}
		params.OnEvent(domain.AgentEvent{
			Type:          domain.EventTurnFailed,
			Timestamp:     now,
			Message:       lastResult.Subtype,
			APIDurationMS: turnAPIDuration,
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

// maxToolErrorLen is the maximum byte budget for [domain.AgentEvent.Message]
// when a tool_result block carries is_error: true. Output that exceeds this
// limit is formatted as first-line-plus-tail: the first line of the error
// output is preserved (typically an exit-code header), followed by the
// omission marker "\n...\n", followed by the last bytes of the remaining
// output — ensuring that CLI failure lines at the tail are always visible.
const maxToolErrorLen = 2048

// toolResultText extracts a human-readable string from a tool_result
// content block. It handles the two shapes emitted by Claude Code:
// a plain JSON string and an array of typed content objects. Returns
// an empty string when neither shape matches or when both Text and
// Content are empty.
func toolResultText(block rawContentBlock) string {
	if block.Text != "" {
		return block.Text
	}
	if len(block.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(block.Content, &s); err == nil && s != "" {
		return s
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(block.Content, &items); err == nil {
		for _, item := range items {
			if item.Type == "text" && item.Text != "" {
				return item.Text
			}
		}
	}
	return ""
}

// stripClaudeMarkup removes Claude Code-specific markup from a raw error
// string before it reaches [domain.AgentEvent.Message]. Two cleanups are
// applied in sequence:
//
//  1. If s is wrapped in a <tool_use_error>…</tool_use_error> envelope
//     (Claude Code's internal error protocol), the envelope is removed and
//     the inner content is returned. Only the outermost wrapper is stripped;
//     inner angle brackets are left intact.
//
//  2. ANSI SGR escape sequences (e.g. \x1b[31m…\x1b[0m) are removed so that
//     structured log fields contain plain text that grep can match directly.
//
// The function degrades gracefully: if neither condition applies the original
// string is returned unchanged.
func stripClaudeMarkup(s string) string {
	s = strings.TrimSpace(s)
	const open = "<tool_use_error>"
	const close = "</tool_use_error>"
	if strings.HasPrefix(s, open) && strings.HasSuffix(s, close) {
		s = strings.TrimSpace(s[len(open) : len(s)-len(close)])
	}
	return ansiEscapeRE.ReplaceAllString(s, "")
}

// tailBytes returns the last n bytes of s aligned to a valid UTF-8 rune
// boundary. If len(s) <= n, s is returned unchanged. When the n-byte suffix
// begins mid-rune, start is advanced past the continuation bytes so the
// result always starts on a rune boundary; the result may be up to 3 bytes
// shorter than n in that case.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// truncateToolError returns s within maxLen bytes, preserving the most useful
// content for operator log inspection. When s fits in maxLen it is returned
// unchanged. For longer strings the algorithm is first-line-plus-tail:
//
//   - The first line of s (up to the first '\n') is kept as a header — for
//     CLI tools this is typically an exit-code line such as "Exit code 2".
//   - The omission marker "\n...\n" separates the header from the tail.
//   - The remaining byte budget after the header and marker is filled by the
//     last bytes of s[after first line], so that failure lines at the tail
//     (e.g. "FAIL pkg 0.5s") are always included.
//
// When no newline is present, or when the first line alone exceeds the budget,
// tailBytes(s, maxLen) is returned instead. All truncation respects UTF-8 rune
// boundaries.
func truncateToolError(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	nlPos := strings.IndexByte(s, '\n')
	if nlPos == -1 {
		return tailBytes(s, maxLen)
	}
	firstLine := s[:nlPos]
	const sep = "\n...\n"
	tailBudget := maxLen - len(firstLine) - len(sep)
	if tailBudget <= 0 {
		return tailBytes(s, maxLen)
	}
	tail := tailBytes(s[nlPos+1:], tailBudget)
	return firstLine + sep + tail
}

// processToolBlocks scans content blocks for tool_use and tool_result
// entries. tool_use blocks are registered in inFlight; tool_result
// blocks are correlated against inFlight and emitted as
// [domain.EventToolResult] via onEvent. The observed parameter is a
// monotonic-capable timestamp for duration math; wallTime is the
// wall-clock timestamp written into emitted events.
func processToolBlocks(
	blocks []rawContentBlock,
	inFlight map[string]inFlightTool,
	observed time.Time,
	wallTime time.Time,
	onEvent func(domain.AgentEvent),
) {
	for _, block := range blocks {
		if block.Type == "tool_use" && block.ID != "" {
			inFlight[block.ID] = inFlightTool{Name: block.Name, Timestamp: observed}
		}
		if block.Type == "tool_result" {
			toolName := "unknown"
			var durationMS int64
			if entry, ok := inFlight[block.ToolUseID]; ok {
				toolName = entry.Name
				if d := observed.Sub(entry.Timestamp); d > 0 {
					durationMS = d.Milliseconds()
				}
				delete(inFlight, block.ToolUseID)
			}
			msg := "tool_result: " + toolName
			if block.IsError {
				if errText := toolResultText(block); errText != "" {
					msg = truncateToolError(stripClaudeMarkup(errText), maxToolErrorLen)
				}
			}
			onEvent(domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      wallTime,
				ToolName:       toolName,
				ToolDurationMS: durationMS,
				ToolError:      block.IsError,
				Message:        msg,
			})
		}
	}
}
