package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// sendRequest writes a JSON-RPC request to the app-server stdin.
// It acquires state.mu internally to protect both the request ID
// counter and the stdin write. Callers must not hold state.mu.
func sendRequest(state *sessionState, method string, params any) (int64, error) {
	state.mu.Lock()
	state.nextRequestID++
	id := state.nextRequestID
	state.mu.Unlock()

	req := rpcRequest{
		Method: method,
		ID:     id,
		Params: params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshal request %s: %w", method, err)
	}
	data = append(data, '\n')

	state.mu.Lock()
	_, writeErr := state.stdin.Write(data)
	state.mu.Unlock()
	if writeErr != nil {
		return 0, fmt.Errorf("write request %s: %w", method, writeErr)
	}
	return id, nil
}

// sendNotification writes a JSON-RPC notification (no id) to the
// app-server stdin.
func sendNotification(state *sessionState, method string, params any) error {
	// Notifications have no id field. Use the rpcRequest type but
	// omit the ID (zero value is omitempty).
	notif := rpcRequest{
		Method: method,
		Params: params,
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshal notification %s: %w", method, err)
	}
	data = append(data, '\n')

	state.mu.Lock()
	_, writeErr := state.stdin.Write(data)
	state.mu.Unlock()
	if writeErr != nil {
		return fmt.Errorf("write notification %s: %w", method, writeErr)
	}
	return nil
}

// sendResponse writes a JSON-RPC response to the app-server stdin.
// The caller must hold state.mu.
func sendResponse(state *sessionState, id int64, result any) error {
	type response struct {
		ID     int64 `json:"id"`
		Result any   `json:"result"`
	}
	resp := response{ID: id, Result: result}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response id=%d: %w", id, err)
	}
	data = append(data, '\n')

	_, writeErr := state.stdin.Write(data)
	if writeErr != nil {
		return fmt.Errorf("write response id=%d: %w", id, writeErr)
	}
	return nil
}

// scanResult carries one line from the background scanner goroutine,
// or a terminal condition (EOF / error).
type scanResult struct {
	Line []byte
	Err  error
	EOF  bool
}

// startScannerCh wraps scanner.Scan in a background goroutine and
// delivers results on the returned channel. The goroutine exits when
// the scanner reaches EOF or returns an error. Closing stop only
// interrupts delivery to scanCh; it does not unblock a scanner.Scan
// call that is blocked on the underlying reader, so shutdown must also
// close that reader or terminate the subprocess. Callers must not
// close the returned channel.
func startScannerCh(scanner *bufio.Scanner, stop <-chan struct{}) <-chan scanResult {
	scanCh := make(chan scanResult, 1)
	go func() {
		defer close(scanCh)
		for scanner.Scan() {
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			select {
			case scanCh <- scanResult{Line: line}:
			case <-stop:
				return
			}
		}
		termResult := scanResult{EOF: true}
		if err := scanner.Err(); err != nil {
			termResult = scanResult{Err: err}
		}
		select {
		case scanCh <- termResult:
		case <-stop:
		}
	}()
	return scanCh
}

// readResponse reads from scanCh until a response with the expected
// ID arrives or the context expires. Notifications encountered during
// the wait are discarded.
func readResponse(ctx context.Context, scanCh <-chan scanResult, expectedID int64) (rpcResponse, error) {
	for {
		select {
		case <-ctx.Done():
			return rpcResponse{}, ctx.Err()
		case result, ok := <-scanCh:
			if !ok || result.EOF {
				return rpcResponse{}, fmt.Errorf("unexpected EOF waiting for response id=%d", expectedID)
			}
			if result.Err != nil {
				return rpcResponse{}, fmt.Errorf("scanner error waiting for response id=%d: %w", expectedID, result.Err)
			}
			msg := parseMessage(result.Line)
			if msg.Err != nil {
				slog.Debug("discarding malformed message during handshake", slog.Any("error", msg.Err))
				continue
			}
			if msg.IsNotification {
				slog.Debug("discarding notification during handshake",
					slog.String("method", msg.Notification.Method))
				continue
			}
			if msg.IsResponse && msg.Response.ID == expectedID {
				return msg.Response, nil
			}
			slog.Debug("discarding response with unexpected id",
				slog.Int64("got", msg.Response.ID),
				slog.Int64("want", expectedID))
		}
	}
}

// initializeHandshake sends the initialize request and initialized
// notification per the app-server protocol.
func initializeHandshake(ctx context.Context, state *sessionState, scanCh <-chan scanResult) error {
	type clientInfo struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	}
	type capabilities struct {
		ExperimentalAPI bool `json:"experimentalApi"`
	}
	type initParams struct {
		ClientInfo   clientInfo   `json:"clientInfo"`
		Capabilities capabilities `json:"capabilities"`
	}

	params := initParams{
		ClientInfo: clientInfo{
			Name:    "sortie_orchestrator",
			Title:   "Sortie Orchestrator",
			Version: "0.1.0",
		},
		Capabilities: capabilities{
			ExperimentalAPI: true,
		},
	}

	id, err := sendRequest(state, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	resp, err := readResponse(ctx, scanCh, id)
	if err != nil {
		return fmt.Errorf("initialize response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	if err := sendNotification(state, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

// authenticateIfNeeded checks the app-server auth state and performs
// API key login if needed.
func authenticateIfNeeded(ctx context.Context, state *sessionState, scanCh <-chan scanResult) error {
	id, err := sendRequest(state, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return fmt.Errorf("account/read: %w", err)
	}

	resp, err := readResponse(ctx, scanCh, id)
	if err != nil {
		return fmt.Errorf("account/read response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("account/read error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var acct accountResult
	if err := json.Unmarshal(resp.Result, &acct); err != nil {
		return fmt.Errorf("account/read unmarshal: %w", err)
	}

	// Account is non-null when the app-server already has valid
	// credentials. The JSON null literal unmarshals to a nil
	// RawMessage.
	if len(acct.Account) > 0 && string(acct.Account) != "null" {
		return nil
	}

	apiKey := os.Getenv("CODEX_API_KEY")
	if apiKey == "" {
		return nil
	}

	loginID, err := sendRequest(state, "account/login/start", map[string]any{
		"type":   "apiKey",
		"apiKey": apiKey,
	})
	if err != nil {
		return fmt.Errorf("account/login/start: %w", err)
	}

	loginResp, err := readResponse(ctx, scanCh, loginID)
	if err != nil {
		return fmt.Errorf("account/login/start response: %w", err)
	}
	if loginResp.Error != nil {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("login failed: %s", loginResp.Error.Message),
		}
	}

	// Wait for account/login/completed notification.
	deadline := time.After(readTimeout(state))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for account/login/completed")
		case result, ok := <-scanCh:
			if !ok || result.EOF {
				return fmt.Errorf("unexpected EOF waiting for login")
			}
			if result.Err != nil {
				return fmt.Errorf("scanner error waiting for login: %w", result.Err)
			}
			msg := parseMessage(result.Line)
			if msg.Err != nil {
				continue
			}
			if msg.IsNotification && msg.Notification.Method == "account/login/completed" {
				var loginNotif accountLoginNotification
				if err := json.Unmarshal(msg.Notification.Params, &loginNotif); err != nil {
					return fmt.Errorf("login notification unmarshal: %w", err)
				}
				if !loginNotif.Success {
					return &domain.AgentError{
						Kind:    domain.ErrResponseError,
						Message: "authentication failed",
					}
				}
				return nil
			}
		}
	}
}

// startThread sends thread/start and waits for the thread/started
// notification. Returns the thread ID.
func startThread(ctx context.Context, state *sessionState, scanCh <-chan scanResult, pt passthroughConfig, tools []domain.AgentTool) (string, error) {
	approvalPolicy := pt.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = "never"
	}

	sandbox := normalizeSandbox(pt.ThreadSandbox)
	if sandbox == "" {
		sandbox = "workspace-write"
	}

	params := map[string]any{
		"cwd":            state.target.WorkspacePath,
		"approvalPolicy": approvalPolicy,
		"sandbox":        sandbox,
	}
	if pt.Model != "" {
		params["model"] = pt.Model
	}
	if pt.Personality != "" {
		params["personality"] = pt.Personality
	}

	dynTools := buildDynamicTools(tools)
	if len(dynTools) > 0 {
		params["dynamicTools"] = dynTools
	}

	id, err := sendRequest(state, "thread/start", params)
	if err != nil {
		return "", fmt.Errorf("thread/start: %w", err)
	}

	resp, err := readResponse(ctx, scanCh, id)
	if err != nil {
		return "", fmt.Errorf("thread/start response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("thread/start error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var result threadResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("thread/start unmarshal: %w", err)
	}
	threadID := result.Thread.ID
	if threadID == "" {
		return "", fmt.Errorf("thread/start returned empty thread ID")
	}

	// Wait for thread/started notification.
	deadline := time.After(readTimeout(state))
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			// Accept the thread ID even without the notification.
			// Some app-server versions may not emit it.
			return threadID, nil
		case result, ok := <-scanCh:
			if !ok || result.EOF {
				return threadID, nil
			}
			if result.Err != nil {
				slog.Debug("scanner error waiting for thread/started", slog.Any("error", result.Err))
				return threadID, nil
			}
			msg := parseMessage(result.Line)
			if msg.Err != nil {
				continue
			}
			if msg.IsNotification && msg.Notification.Method == "thread/started" {
				return threadID, nil
			}
		}
	}
}

// resumeThread sends thread/resume for an existing thread.
func resumeThread(ctx context.Context, state *sessionState, scanCh <-chan scanResult, threadID string) error {
	id, err := sendRequest(state, "thread/resume", map[string]any{
		"threadId": threadID,
	})
	if err != nil {
		return fmt.Errorf("thread/resume: %w", err)
	}

	resp, err := readResponse(ctx, scanCh, id)
	if err != nil {
		return fmt.Errorf("thread/resume response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("thread/resume error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

// buildDynamicTools converts registered agent tools into the schema
// map array required by thread/start.
func buildDynamicTools(tools []domain.AgentTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	result := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		entry := map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
		}
		schema := t.InputSchema()
		if len(schema) > 0 {
			var parsed any
			if err := json.Unmarshal(schema, &parsed); err == nil {
				entry["inputSchema"] = parsed
			}
		}
		result = append(result, entry)
	}
	return result
}

// buildSandboxPolicy constructs the sandboxPolicy for turn/start.
// writableRoots defaults to the workspace path and networkAccess
// defaults to false. Operator overrides from TurnSandboxPolicy
// (WORKFLOW.md turn_sandbox_policy) are merged on top and may
// replace any key, including writableRoots and networkAccess.
func buildSandboxPolicy(state *sessionState, pt passthroughConfig) map[string]any {
	sandboxType := denormalizeSandbox(pt.ThreadSandbox)
	if sandboxType == "" {
		sandboxType = "workspaceWrite"
	}

	policy := map[string]any{
		"type":          sandboxType,
		"writableRoots": []string{state.target.WorkspacePath},
		"networkAccess": false,
	}

	if pt.TurnSandboxPolicy != nil {
		for k, v := range pt.TurnSandboxPolicy {
			policy[k] = v
		}
	}
	return policy
}

// normalizeSandbox maps user-friendly camelCase sandbox values from
// WORKFLOW.md to the kebab-case wire format expected by the app-server
// thread/start sandbox field. Values already in kebab-case are passed
// through unchanged.
func normalizeSandbox(s string) string {
	switch s {
	case "workspaceWrite":
		return "workspace-write"
	case "readOnly":
		return "read-only"
	case "dangerFullAccess":
		return "danger-full-access"
	case "externalSandbox":
		return "external-sandbox"
	default:
		return s
	}
}

// denormalizeSandbox maps kebab-case sandbox values to the camelCase wire
// format expected by the app-server turn/start sandboxPolicy.type field.
// Values already in camelCase are passed through unchanged.
func denormalizeSandbox(s string) string {
	switch s {
	case "workspace-write":
		return "workspaceWrite"
	case "read-only":
		return "readOnly"
	case "danger-full-access":
		return "dangerFullAccess"
	case "external-sandbox":
		return "externalSandbox"
	default:
		return s
	}
}

// readTimeout returns the read timeout duration from the agent config,
// defaulting to 30 seconds.
func readTimeout(state *sessionState) time.Duration {
	if state.agentConfig.ReadTimeoutMS > 0 {
		return time.Duration(state.agentConfig.ReadTimeoutMS) * time.Millisecond
	}
	return 30 * time.Second
}
