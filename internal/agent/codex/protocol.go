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
// The caller must hold state.mu when concurrent writes are possible.
// Returns the request ID used.
func sendRequest(state *sessionState, method string, params any) (int64, error) {
	state.nextRequestID++
	id := state.nextRequestID
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

// readResponse reads JSONL lines from the scanner until a response
// with the expected ID arrives or the context expires. Notifications
// encountered during the wait are discarded.
func readResponse(ctx context.Context, scanner *bufio.Scanner, expectedID int64) (rpcResponse, error) {
	for {
		if ctx.Err() != nil {
			return rpcResponse{}, ctx.Err()
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return rpcResponse{}, fmt.Errorf("scanner error waiting for response id=%d: %w", expectedID, err)
			}
			return rpcResponse{}, fmt.Errorf("unexpected EOF waiting for response id=%d", expectedID)
		}

		msg := parseMessage(scanner.Bytes())
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

// initializeHandshake sends the initialize request and initialized
// notification per the app-server protocol.
func initializeHandshake(ctx context.Context, state *sessionState, scanner *bufio.Scanner) error {
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

	resp, err := readResponse(ctx, scanner, id)
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
func authenticateIfNeeded(ctx context.Context, state *sessionState, scanner *bufio.Scanner) error {
	id, err := sendRequest(state, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return fmt.Errorf("account/read: %w", err)
	}

	resp, err := readResponse(ctx, scanner, id)
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

	// Read until login response and login/completed notification.
	loginResp, err := readResponse(ctx, scanner, loginID)
	if err != nil {
		return fmt.Errorf("account/login/start response: %w", err)
	}
	if loginResp.Error != nil {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("login failed: %s", loginResp.Error.Message),
		}
	}

	// Wait for account/login/completed notification. Read lines
	// until we find it.
	deadline := time.After(readTimeout(state))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for account/login/completed")
		default:
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("scanner error waiting for login: %w", err)
			}
			return fmt.Errorf("unexpected EOF waiting for login")
		}
		msg := parseMessage(scanner.Bytes())
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

// startThread sends thread/start and waits for the thread/started
// notification. Returns the thread ID.
func startThread(ctx context.Context, state *sessionState, scanner *bufio.Scanner, pt passthroughConfig, tools []domain.AgentTool) (string, error) {
	approvalPolicy := pt.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = "never"
	}

	sandbox := pt.ThreadSandbox
	if sandbox == "" {
		sandbox = "workspaceWrite"
	}

	params := map[string]any{
		"cwd":            state.workspacePath,
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

	resp, err := readResponse(ctx, scanner, id)
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
		default:
		}
		if !scanner.Scan() {
			return threadID, nil
		}
		msg := parseMessage(scanner.Bytes())
		if msg.Err != nil {
			continue
		}
		if msg.IsNotification && msg.Notification.Method == "thread/started" {
			return threadID, nil
		}
	}
}

// resumeThread sends thread/resume for an existing thread.
func resumeThread(ctx context.Context, state *sessionState, scanner *bufio.Scanner, threadID string) error {
	id, err := sendRequest(state, "thread/resume", map[string]any{
		"threadId": threadID,
	})
	if err != nil {
		return fmt.Errorf("thread/resume: %w", err)
	}

	resp, err := readResponse(ctx, scanner, id)
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
// writableRoots is always set to the workspace path. networkAccess
// defaults to false. Operator overrides from TurnSandboxPolicy are
// merged on top.
func buildSandboxPolicy(state *sessionState, pt passthroughConfig) map[string]any {
	sandboxType := pt.ThreadSandbox
	if sandboxType == "" {
		sandboxType = "workspaceWrite"
	}

	policy := map[string]any{
		"type":          sandboxType,
		"writableRoots": []string{state.workspacePath},
		"networkAccess": false,
	}

	if pt.TurnSandboxPolicy != nil {
		for k, v := range pt.TurnSandboxPolicy {
			policy[k] = v
		}
	}
	return policy
}

// readTimeout returns the read timeout duration from the agent config,
// defaulting to 30 seconds.
func readTimeout(state *sessionState) time.Duration {
	if state.agentConfig.ReadTimeoutMS > 0 {
		return time.Duration(state.agentConfig.ReadTimeoutMS) * time.Millisecond
	}
	return 30 * time.Second
}
