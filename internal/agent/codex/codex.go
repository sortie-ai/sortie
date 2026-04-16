// Package codex implements [domain.AgentAdapter] for the OpenAI Codex
// CLI. It launches `codex app-server` as a persistent subprocess,
// communicates via JSON-RPC 2.0 over stdin/stdout (JSONL), and
// normalizes events into domain types. Registered under kind "codex"
// via an init function. Unlike the Claude Code and Copilot adapters,
// the subprocess persists across turns within a session.
package codex

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("codex", NewCodexAdapter, registry.AgentMeta{
		RequiresCommand: true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CodexAdapter)(nil)

// inFlightTool tracks a tool execution that has started but whose
// corresponding item/completed has not yet arrived.
type inFlightTool struct {
	Name      string
	Timestamp time.Time
}

// CodexAdapter satisfies [domain.AgentAdapter] by managing a persistent
// codex app-server subprocess. One adapter instance serves all
// concurrent sessions; per-session state is held in [sessionState] via
// the [domain.Session] Internal field.
type CodexAdapter struct {
	passthrough  passthroughConfig
	toolRegistry *domain.ToolRegistry
}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the persistent app-server subprocess, thread
// ID, and turn state across the session lifetime.
type sessionState struct {
	workspacePath string
	command       string
	agentConfig   domain.AgentConfig
	turnCount     int

	threadID      string
	nextRequestID int64

	sshHost                  string
	remoteCommand            string
	sshStrictHostKeyChecking string
	mcpConfigPath            string

	// mu guards proc, waitCh, stdin, stdout, and stderrCollector for
	// concurrent access from StopSession and the event read loop.
	mu              sync.Mutex
	proc            *os.Process
	waitCh          chan struct{}
	stdin           io.WriteCloser
	stdout          io.ReadCloser
	stderrCollector *procutil.StderrCollector

	// Session-scoped reader channels. The reader goroutine reads
	// stdout after the handshake and delivers parsed messages to
	// RunTurn via msgCh. stopCh is closed by StopSession to unblock
	// the reader if msgCh is full. readerDone is closed by the reader
	// when it exits. closeStop guards against double-closing stopCh
	// when StopSession is called more than once.
	msgCh      chan parsedMessage
	readerDone chan struct{}
	stopCh     chan struct{}
	closeStop  sync.Once
}

// NewCodexAdapter creates a [CodexAdapter] from adapter configuration.
// The config parameter is the raw map from the "codex" sub-object in
// WORKFLOW.md. Command resolution is deferred to
// [CodexAdapter.StartSession].
func NewCodexAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt := parsePassthroughConfig(config)
	adapter := &CodexAdapter{passthrough: pt}

	if tr, ok := config["tool_registry"]; ok {
		if reg, ok := tr.(*domain.ToolRegistry); ok {
			adapter.toolRegistry = reg
		}
	}

	return adapter, nil
}

// StartSession validates the workspace path, resolves the codex
// binary, launches the app-server subprocess, performs the
// initialization handshake, authenticates if needed, and starts or
// resumes a thread.
func (a *CodexAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
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

	fi, err := os.Stat(absPath)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path does not exist",
			Err:     err,
		}
	}
	if !fi.IsDir() {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path is not a directory",
		}
	}

	command := cmp.Or(params.AgentConfig.Command, "codex app-server")

	state := &sessionState{
		workspacePath:            absPath,
		agentConfig:              params.AgentConfig,
		sshStrictHostKeyChecking: params.SSHStrictHostKeyChecking,
		mcpConfigPath:            params.MCPConfigPath,
	}

	var cmdPath string
	var cmdArgs []string

	sshHostTrimmed := strings.TrimSpace(params.SSHHost)
	if sshHostTrimmed != "" {
		sshPath, lookErr := exec.LookPath("ssh")
		if lookErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "ssh binary not found on orchestrator host",
				Err:     lookErr,
			}
		}

		// Build the remote command, prefixing CODEX_API_KEY if present
		// so it reaches the remote shell.
		remoteCmd := command
		if apiKey := os.Getenv("CODEX_API_KEY"); apiKey != "" {
			remoteCmd = fmt.Sprintf("CODEX_API_KEY=%s %s", sshutil.ShellQuote(apiKey), command)
		}

		sshArgs := sshutil.BuildSSHArgs(sshHostTrimmed, absPath, remoteCmd, nil, sshutil.SSHOptions{
			StrictHostKeyChecking: params.SSHStrictHostKeyChecking,
		})

		cmdPath = sshPath
		cmdArgs = sshArgs
		state.command = sshPath
		state.sshHost = sshHostTrimmed
		state.remoteCommand = command
	} else {
		// Local mode: the command may be "codex app-server" (with args).
		// Split on first space to extract binary and arguments.
		parts := strings.Fields(command)
		if len(parts) == 0 {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "agent command is empty or whitespace-only",
			}
		}
		resolved, lookErr := exec.LookPath(parts[0])
		if lookErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: fmt.Sprintf("agent command %q not found", parts[0]),
				Err:     lookErr,
			}
		}
		cmdPath = resolved
		if len(parts) > 1 {
			cmdArgs = parts[1:]
		}
		state.command = resolved
	}

	cmd := exec.CommandContext(ctx, cmdPath, cmdArgs...) //nolint:gosec // args are constructed programmatically
	procutil.SetProcessGroup(cmd)
	cmd.Dir = absPath
	cmd.Env = os.Environ()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdin pipe",
			Err:     err,
		}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdout pipe",
			Err:     err,
		}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stderr pipe",
			Err:     err,
		}
	}

	if err := cmd.Start(); err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to start app-server subprocess",
			Err:     err,
		}
	}

	logger := slog.Default().With(slog.String("component", "codex-adapter"))
	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}

	state.proc = cmd.Process
	state.stdin = stdinPipe
	state.stdout = stdoutPipe
	state.waitCh = make(chan struct{})
	state.stderrCollector = procutil.NewStderrCollector(stderrPipe, logger)

	// Background goroutine to close waitCh when the process exits.
	go func() {
		cmd.Wait()                                 //nolint:errcheck,gosec // exit code handled via waitCh
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)
		close(state.waitCh)
	}()

	// killOnError is a cleanup closure used if any handshake step fails.
	killOnError := func() {
		state.mu.Lock()
		if state.stdin != nil {
			state.stdin.Close() //nolint:errcheck,gosec // best-effort cleanup
		}
		state.mu.Unlock()
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup
		// Wait briefly for cleanup.
		select {
		case <-state.waitCh:
		case <-time.After(3 * time.Second):
		}
		state.mu.Lock()
		state.proc = nil
		state.stdin = nil
		state.stdout = nil
		state.mu.Unlock()
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	// Create stopCh before the scanner goroutine so it is available
	// to startScannerCh and to handshake error paths.
	state.stopCh = make(chan struct{})
	scanCh := startScannerCh(scanner, state.stopCh)

	if err := initializeHandshake(ctx, state, scanCh); err != nil {
		state.closeStop.Do(func() { close(state.stopCh) })
		killOnError()
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("handshake failed: %v", err),
			Err:     err,
		}
	}

	if err := authenticateIfNeeded(ctx, state, scanCh); err != nil {
		state.closeStop.Do(func() { close(state.stopCh) })
		killOnError()
		var agentErr *domain.AgentError
		if ok := isAgentError(err, &agentErr); ok {
			return domain.Session{}, agentErr
		}
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("authentication failed: %v", err),
			Err:     err,
		}
	}

	var threadID string
	if params.ResumeSessionID != "" {
		if err := resumeThread(ctx, state, scanCh, params.ResumeSessionID); err != nil {
			// Fallback to new thread on resume failure.
			logger.Warn("thread resume failed, starting new thread",
				slog.String("resume_id", params.ResumeSessionID),
				slog.Any("error", err))
			var tools []domain.AgentTool
			if a.toolRegistry != nil {
				tools = a.toolRegistry.List()
			}
			tid, startErr := startThread(ctx, state, scanCh, a.passthrough, tools)
			if startErr != nil {
				state.closeStop.Do(func() { close(state.stopCh) })
				killOnError()
				return domain.Session{}, &domain.AgentError{
					Kind:    domain.ErrResponseError,
					Message: fmt.Sprintf("thread/start failed: %v", startErr),
					Err:     startErr,
				}
			}
			threadID = tid
		} else {
			threadID = params.ResumeSessionID
		}
	} else {
		var tools []domain.AgentTool
		if a.toolRegistry != nil {
			tools = a.toolRegistry.List()
		}
		tid, startErr := startThread(ctx, state, scanCh, a.passthrough, tools)
		if startErr != nil {
			state.closeStop.Do(func() { close(state.stopCh) })
			killOnError()
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: fmt.Sprintf("thread/start failed: %v", startErr),
				Err:     startErr,
			}
		}
		threadID = tid
	}

	state.threadID = threadID

	state.msgCh = make(chan parsedMessage, 16)
	state.readerDone = make(chan struct{})

	go func() {
		defer close(state.readerDone)
		defer close(state.msgCh)
		for result := range scanCh {
			if result.EOF || result.Err != nil {
				if result.Err != nil {
					select {
					case state.msgCh <- parsedMessage{Err: result.Err}:
					case <-state.stopCh:
					}
				}
				return
			}
			msg := parseMessage(result.Line)
			select {
			case state.msgCh <- msg:
			case <-state.stopCh:
				return
			}
		}
	}()

	return domain.Session{
		ID:       threadID,
		AgentPID: strconv.Itoa(cmd.Process.Pid),
		Internal: state,
	}, nil
}

// RunTurn sends a turn/start request on the existing thread and reads
// events until turn/completed. Events are delivered synchronously via
// params.OnEvent.
func (a *CodexAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("codex: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	logger := logging.WithSession(
		slog.Default().With(slog.String("component", "codex-adapter")),
		state.threadID,
	)

	state.turnCount++

	// Build turn/start params.
	turnParams := map[string]any{
		"threadId": state.threadID,
		"input":    []map[string]any{{"type": "text", "text": params.Prompt}},
		"cwd":      state.workspacePath,
	}

	if state.turnCount == 1 || a.passthrough.TurnSandboxPolicy != nil {
		turnParams["sandboxPolicy"] = buildSandboxPolicy(state, a.passthrough)
	}
	if a.passthrough.Model != "" {
		turnParams["model"] = a.passthrough.Model
	}
	if a.passthrough.Effort != "" {
		turnParams["effort"] = a.passthrough.Effort
	}

	id, err := sendRequest(state, "turn/start", turnParams)
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("turn/start failed: %v", err),
			Err:     err,
		}
	}

	// Fast-path: return immediately if the context is already done.
	if ctx.Err() != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("turn/start response: %v", ctx.Err()),
			Err:     ctx.Err(),
		}
	}

	// Wait for the turn/start response from the session-scoped reader.
	var turnStartResp rpcResponse
	for turnStartResp.ID == 0 {
		select {
		case <-ctx.Done():
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: fmt.Sprintf("turn/start response: %v", ctx.Err()),
				Err:     ctx.Err(),
			}
		case msg, ok := <-state.msgCh:
			if !ok {
				return domain.TurnResult{}, &domain.AgentError{
					Kind:    domain.ErrPortExit,
					Message: "stdout closed before turn/start response",
				}
			}
			if msg.Err != nil {
				logger.Warn("ignoring unparseable stdout line", slog.Any("error", msg.Err))
				continue
			}
			if msg.IsResponse && msg.Response.ID == id {
				turnStartResp = msg.Response
			}
		}
	}
	if turnStartResp.Error != nil {
		return domain.TurnResult{
				SessionID:  state.threadID,
				ExitReason: domain.EventTurnFailed,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: fmt.Sprintf("turn/start error: %s", turnStartResp.Error.Message),
			}
	}

	var turnResult turnStartResult
	if err := json.Unmarshal(turnStartResp.Result, &turnResult); err != nil {
		logger.Warn("turn/start result unmarshal failed", slog.Any("error", err))
	}
	turnID := turnResult.Turn.ID

	inFlight := make(map[string]inFlightTool)
	var usage domain.TokenUsage
	var toolWg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)
	interrupted := false

	for {
		select {
		case evt := <-toolEventCh:
			params.OnEvent(evt)

		case <-ctx.Done():
			if !interrupted {
				interrupted = true
				// Send turn/interrupt using a detached context so the
				// request is not dropped by the already-cancelled
				// parent context.
				interruptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				sendRequest(state, "turn/interrupt", map[string]any{ //nolint:errcheck,gosec // best-effort interrupt
					"threadId": state.threadID,
					"turnId":   turnID,
				})
				cancel()
				_ = interruptCtx
			}
			// Continue reading until turn/completed or channel close.
			continue

		case msg, ok := <-state.msgCh:
			if !ok {
				// Channel closed — subprocess stdout ended.
				go func() { toolWg.Wait(); close(toolEventCh) }()
				for evt := range toolEventCh {
					params.OnEvent(evt)
				}
				return domain.TurnResult{
						SessionID:  state.threadID,
						ExitReason: domain.EventTurnFailed,
						Usage:      usage,
					}, &domain.AgentError{
						Kind:    domain.ErrPortExit,
						Message: "subprocess stdout closed unexpectedly",
					}
			}
			if msg.Err != nil {
				now := time.Now().UTC()
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventTurnFailed,
					Timestamp: now,
					Message:   msg.Err.Error(),
				})
				go func() { toolWg.Wait(); close(toolEventCh) }()
				for evt := range toolEventCh {
					params.OnEvent(evt)
				}
				return domain.TurnResult{
						SessionID:  state.threadID,
						ExitReason: domain.EventTurnFailed,
						Usage:      usage,
					}, &domain.AgentError{
						Kind:    domain.ErrPortExit,
						Message: fmt.Sprintf("stdout read error: %v", msg.Err),
						Err:     msg.Err,
					}
			}

			// Response messages (echoed tool-call confirmations).
			if msg.IsResponse && !msg.IsNotification {
				continue
			}

			if !msg.IsNotification {
				continue
			}

			now := time.Now().UTC()
			method := msg.Notification.Method

			switch method {
			case "turn/started":
				if state.turnCount == 1 {
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventSessionStarted,
						Timestamp: now,
						SessionID: state.threadID,
						AgentPID:  session.AgentPID,
						Message:   "session started",
					})
				} else {
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   "turn started",
					})
				}

			case "turn/completed":
				var tc turnCompletedParams
				if err := json.Unmarshal(msg.Notification.Params, &tc); err != nil {
					logger.Warn("turn/completed unmarshal failed", slog.Any("error", err))
				}

				if tc.Usage != nil {
					usage = normalizeUsage(tc.Usage)
				}
				exitReason := mapTurnStatus(tc.Turn.Status)

				if tc.Turn.Status == "failed" && tc.Turn.Error != nil {
					kind := mapCodexErrorInfo(tc.Turn.Error.CodexErrorInfo)
					errMsg := tc.Turn.Error.Message
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventTurnFailed,
						Timestamp: now,
						Message:   errMsg,
					})
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventTokenUsage,
						Timestamp: now,
						Usage:     usage,
					})
					go func() { toolWg.Wait(); close(toolEventCh) }()
					for evt := range toolEventCh {
						params.OnEvent(evt)
					}
					return domain.TurnResult{
							SessionID:  state.threadID,
							ExitReason: exitReason,
							Usage:      usage,
						}, &domain.AgentError{
							Kind:    kind,
							Message: errMsg,
						}
				}

				params.OnEvent(domain.AgentEvent{
					Type:      exitReason,
					Timestamp: now,
					Message:   "turn " + tc.Turn.Status,
				})
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventTokenUsage,
					Timestamp: now,
					Usage:     usage,
				})

				go func() { toolWg.Wait(); close(toolEventCh) }()
				for evt := range toolEventCh {
					params.OnEvent(evt)
				}
				return domain.TurnResult{
					SessionID:  state.threadID,
					ExitReason: exitReason,
					Usage:      usage,
				}, nil

			case "item/started":
				var ip itemParams
				if err := json.Unmarshal(msg.Notification.Params, &ip); err != nil {
					logger.Debug("item/started unmarshal failed", slog.Any("error", err))
					continue
				}
				item := ip.Item
				switch item.Type {
				case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
					toolName := cmp.Or(item.Command, item.Type)
					inFlight[item.ID] = inFlightTool{
						Name:      toolName,
						Timestamp: time.Now(),
					}
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   summarizeItem(item.Type, item.ID),
					})
				default:
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   summarizeItem(item.Type, item.ID),
					})
				}

			case "item/completed":
				var ip itemParams
				if err := json.Unmarshal(msg.Notification.Params, &ip); err != nil {
					logger.Debug("item/completed unmarshal failed", slog.Any("error", err))
					continue
				}
				item := ip.Item
				if flight, exists := inFlight[item.ID]; exists {
					dur := time.Since(flight.Timestamp).Milliseconds()
					params.OnEvent(domain.AgentEvent{
						Type:           domain.EventToolResult,
						Timestamp:      now,
						ToolName:       flight.Name,
						ToolDurationMS: dur,
					})
					delete(inFlight, item.ID)
				}
				if item.Type == "agentMessage" && item.Text != "" {
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   truncate(item.Text, 200),
					})
				}

			case "item/agentMessage/delta", "item/commandExecution/outputDelta":
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventNotification,
					Timestamp: now,
				})

			case "item/tool/call":
				if evt := a.handleToolCall(ctx, state, &toolWg, msg, toolEventCh, logger); evt != nil {
					params.OnEvent(*evt)
				}

			case "turn/plan/updated":
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventNotification,
					Timestamp: now,
					Message:   "plan updated",
				})

			case "turn/diff/updated":
				logger.Debug("diff updated")

			default:
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventOtherMessage,
					Timestamp: now,
					Message:   method,
				})
			}
		}
	}
}

// handleToolCall dispatches a dynamic tool call from the app-server to
// the ToolRegistry. The tool is executed asynchronously to avoid
// blocking the event read loop. The provided WaitGroup is incremented
// before launching the goroutine so RunTurn can wait for in-flight
// tools before returning. Asynchronous tool completion events are sent
// via toolEventCh. Synchronous early-return events (unsupported tool)
// are returned directly so the caller can deliver them without risking
// a channel send from the reader goroutine.
func (a *CodexAdapter) handleToolCall(ctx context.Context, state *sessionState, wg *sync.WaitGroup, msg parsedMessage, toolEventCh chan<- domain.AgentEvent, logger *slog.Logger) *domain.AgentEvent {
	now := time.Now().UTC()
	requestID := msg.Response.ID

	var tc toolCallParams
	if err := json.Unmarshal(msg.Notification.Params, &tc); err != nil {
		logger.Warn("item/tool/call unmarshal failed", slog.Any("error", err))
		state.mu.Lock()
		sendResponse(state, requestID, toolResultFor(false, "invalid tool call params")) //nolint:errcheck,gosec // best-effort error response
		state.mu.Unlock()
		return nil
	}

	toolName := tc.Tool

	if a.toolRegistry == nil {
		state.mu.Lock()
		sendResponse(state, requestID, toolResultFor(false, fmt.Sprintf("unsupported tool: %s", toolName))) //nolint:errcheck,gosec // best-effort error response
		state.mu.Unlock()
		return &domain.AgentEvent{
			Type:      domain.EventUnsupportedToolCall,
			Timestamp: now,
			ToolName:  toolName,
			Message:   fmt.Sprintf("no tool registry configured for tool %q", toolName),
		}
	}

	tool, found := a.toolRegistry.Get(toolName)
	if !found {
		state.mu.Lock()
		sendResponse(state, requestID, toolResultFor(false, fmt.Sprintf("unsupported tool: %s", toolName))) //nolint:errcheck,gosec // best-effort error response
		state.mu.Unlock()
		return &domain.AgentEvent{
			Type:      domain.EventUnsupportedToolCall,
			Timestamp: now,
			ToolName:  toolName,
			Message:   fmt.Sprintf("tool %q not registered", toolName),
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		result, execErr := tool.Execute(ctx, tc.Arguments)

		state.mu.Lock()
		if execErr != nil {
			sendResponse(state, requestID, toolResultFor(false, execErr.Error())) //nolint:errcheck,gosec // best-effort error response
		} else {
			sendResponse(state, requestID, toolResultFor(true, string(result))) //nolint:errcheck,gosec // best-effort success response
		}
		state.mu.Unlock()

		if execErr != nil {
			toolEventCh <- domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      time.Now().UTC(),
				ToolName:       toolName,
				ToolDurationMS: time.Since(start).Milliseconds(),
				ToolError:      true,
				Message:        execErr.Error(),
			}
		} else {
			toolEventCh <- domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      time.Now().UTC(),
				ToolName:       toolName,
				ToolDurationMS: time.Since(start).Milliseconds(),
			}
		}
	}()
	return nil
}

// StopSession terminates the persistent app-server subprocess.
func (a *CodexAdapter) StopSession(_ context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}

	// Signal the reader goroutine to stop before closing stdin,
	// preventing it from blocking on a full msgCh during teardown.
	state.closeStop.Do(func() {
		if state.stopCh != nil {
			close(state.stopCh)
		}
	})

	// Close stdin to signal EOF to the app-server.
	state.mu.Lock()
	if state.stdin != nil {
		state.stdin.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
	waitCh := state.waitCh
	pid := 0
	if state.proc != nil {
		pid = state.proc.Pid
	}
	state.mu.Unlock()

	if pid > 0 {
		procutil.SignalGraceful(pid) //nolint:errcheck,gosec // best-effort graceful shutdown
	}

	// Wait for process exit with a 5-second grace period.
	if waitCh != nil {
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			if pid > 0 {
				procutil.KillProcessGroup(pid) //nolint:errcheck,gosec // best-effort force kill
			}
			// Wait again briefly for cleanup.
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
			}
		}
	}

	// Wait for the reader goroutine to finish after process exit.
	if state.readerDone != nil {
		select {
		case <-state.readerDone:
		case <-time.After(2 * time.Second):
			logger := logging.WithSession(
				slog.Default().With(slog.String("component", "codex-adapter")),
				state.threadID,
			)
			logger.Warn("reader goroutine did not exit after process termination")
		}
	}

	state.mu.Lock()
	state.proc = nil
	state.stdin = nil
	state.stdout = nil
	state.waitCh = nil
	state.mu.Unlock()

	return nil
}

// EventStream returns nil. The Codex adapter delivers events
// synchronously via [CodexAdapter.RunTurn]'s OnEvent callback.
func (a *CodexAdapter) EventStream() <-chan domain.AgentEvent { return nil }

// isAgentError extracts an *[domain.AgentError] from err using type
// assertion.
func isAgentError(err error, target **domain.AgentError) bool {
	ae, ok := err.(*domain.AgentError) //nolint:errorlint // direct type check is intentional
	if ok {
		*target = ae
		return true
	}
	return false
}
