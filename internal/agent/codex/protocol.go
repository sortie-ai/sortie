package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// initializeHandshake sends the initialize request and initialized
// notification per the app-server protocol.
func initializeHandshake(ctx context.Context, state *sessionState) error {
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

	resp, err := state.conn.Call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	if err := state.conn.Notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return nil
}

// authenticateIfNeeded checks the app-server auth state and performs
// API key login if needed.
func authenticateIfNeeded(ctx context.Context, state *sessionState) error {
	resp, err := state.conn.Call(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return fmt.Errorf("account/read: %w", err)
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

	loginResp, err := state.conn.Call(ctx, "account/login/start", map[string]any{
		"type":   "apiKey",
		"apiKey": apiKey,
	})
	if err != nil {
		return fmt.Errorf("account/login/start: %w", err)
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
		case msg, ok := <-state.msgCh:
			if !ok {
				return fmt.Errorf("unexpected EOF waiting for login")
			}
			if msg.Kind == jsonrpc.KindStreamEnd {
				return fmt.Errorf("scanner error waiting for login: %w", msg.Err)
			}
			if msg.Kind == jsonrpc.KindMalformed {
				continue
			}
			if msg.Kind == jsonrpc.KindNotification && msg.Method == "account/login/completed" {
				var loginNotif accountLoginNotification
				if err := json.Unmarshal(msg.Params, &loginNotif); err != nil {
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
// notification. Returns the thread ID and the effective model the
// response reported, empty when the response omitted it. An empty
// model is never an error.
func startThread(ctx context.Context, state *sessionState, pt passthroughConfig) (threadID string, model string, err error) {
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

	resp, callErr := state.conn.Call(ctx, "thread/start", params)
	if callErr != nil {
		return "", "", fmt.Errorf("thread/start: %w", callErr)
	}
	if resp.Error != nil {
		return "", "", fmt.Errorf("thread/start error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var result threadResult
	if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr != nil {
		return "", "", fmt.Errorf("thread/start unmarshal: %w", unmarshalErr)
	}
	threadID = result.Thread.ID
	if threadID == "" {
		return "", "", fmt.Errorf("thread/start returned empty thread ID")
	}

	// Wait for thread/started notification.
	deadline := time.After(readTimeout(state))
	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-deadline:
			// Accept the thread ID even without the notification.
			// Some app-server versions may not emit it.
			return threadID, result.Model, nil
		case msg, ok := <-state.msgCh:
			if !ok {
				return threadID, result.Model, nil
			}
			if msg.Kind == jsonrpc.KindStreamEnd {
				slog.Debug("scanner error waiting for thread/started", slog.Any("error", msg.Err))
				return threadID, result.Model, nil
			}
			if msg.Kind == jsonrpc.KindMalformed {
				continue
			}
			if msg.Kind == jsonrpc.KindNotification && msg.Method == "thread/started" {
				return threadID, result.Model, nil
			}
		}
	}
}

// resumeThread sends thread/resume for an existing thread. It returns
// the effective model the response reported. A response whose result
// fails to unmarshal does not turn a successful resume into a
// failure: it returns an empty model and a nil error.
func resumeThread(ctx context.Context, state *sessionState, threadID string) (model string, err error) {
	resp, callErr := state.conn.Call(ctx, "thread/resume", map[string]any{
		"threadId": threadID,
	})
	if callErr != nil {
		return "", fmt.Errorf("thread/resume: %w", callErr)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("thread/resume error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var result threadResult
	if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr != nil {
		return "", nil
	}
	return result.Model, nil
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
		maps.Copy(policy, pt.TurnSandboxPolicy)
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
