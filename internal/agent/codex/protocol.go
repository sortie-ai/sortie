package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/logging"
)

// errConnectionLost marks a startup handshake error, for errors.Is, when
// the app-server connection ended without answering.
var errConnectionLost = errors.New("codex: connection lost during startup")

// connectionLossErr renders exactly the wrapped error's text: when the
// runtime outlives the early-exit observation, the caller reports that
// text unchanged.
type connectionLossErr struct {
	err error
}

func (e *connectionLossErr) Error() string        { return e.err.Error() }
func (e *connectionLossErr) Unwrap() error        { return e.err }
func (e *connectionLossErr) Is(target error) bool { return target == errConnectionLost }

// wrapConnectionLoss marks err as a connection loss unless it is a
// cancelled or elapsed context, whose cause the caller already knows.
func wrapConnectionLoss(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &connectionLossErr{err: err}
}

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

	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()
	resp, err := state.conn.Call(callCtx, "initialize", params)
	if err != nil {
		return wrapConnectionLoss(fmt.Errorf("initialize: %w", err))
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	if err := state.conn.Notify("initialized", map[string]any{}); err != nil {
		return wrapConnectionLoss(fmt.Errorf("initialized notification: %w", err))
	}
	return nil
}

// authenticateIfNeeded logs in with CODEX_API_KEY when the app-server
// holds no account and the key is set. logger carries no session scope:
// no thread exists yet.
func authenticateIfNeeded(ctx context.Context, state *sessionState, logger *slog.Logger) error {
	readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()
	resp, err := state.conn.Call(readCtx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return wrapConnectionLoss(fmt.Errorf("account/read: %w", err))
	}
	if resp.Error != nil {
		return fmt.Errorf("account/read error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var acct accountResult
	if err := json.Unmarshal(resp.Result, &acct); err != nil {
		return fmt.Errorf("account/read unmarshal: %w", err)
	}

	if hasAccount(acct) {
		return nil
	}

	apiKey := os.Getenv("CODEX_API_KEY")
	if apiKey == "" {
		return nil
	}

	loginCtx, loginCancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	loginResp, err := state.conn.Call(loginCtx, "account/login/start", map[string]any{
		"type":   "apiKey",
		"apiKey": apiKey,
	})
	loginCancel()
	if err != nil {
		return wrapConnectionLoss(fmt.Errorf("account/login/start: %w", err))
	}
	if loginResp.Error != nil {
		text := loginResp.Error.Message
		if text == "" {
			text = "the login did not succeed"
		}
		return agentcore.CredentialRefusedError(text, nil)
	}
	return awaitLoginCompleted(ctx, state, logger)
}

func hasAccount(acct accountResult) bool {
	return len(acct.Account) > 0 && string(acct.Account) != "null"
}

func awaitLoginCompleted(ctx context.Context, state *sessionState, logger *slog.Logger) error {
	deadline := time.After(agentcore.ReadTimeout(state.agentConfig))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for account/login/completed")
		case <-state.inbox.Ready():
			msg, ok := state.inbox.Take()
			if !ok {
				return wrapConnectionLoss(fmt.Errorf("unexpected EOF waiting for login"))
			}
			if msg.Kind == jsonrpc.KindStreamEnd {
				return wrapConnectionLoss(fmt.Errorf("scanner error waiting for login: %w", msg.Err))
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
					text := loginNotif.Error
					if text == "" {
						text = "the login did not succeed"
					}
					return agentcore.CredentialRefusedError(text, nil)
				}
				return nil
			}
			handleOutOfTurnMessage(state, msg, logger)
		}
	}
}

// verificationDisabledFeatures are flags from the runtime's published
// configuration reference; a build that does not know one ignores it.
var verificationDisabledFeatures = []string{
	"shell_tool", "unified_exec", "multi_agent", "apps", "plugins", "goals",
	"browser_use", "computer_use", "hooks", "in_app_browser",
}

// verificationThreadConfig uses flat, dotted keys and gives each value the JSON
// type the configuration reference gives it: a wrong type fails thread/start,
// where an unknown key is only ignored.
func verificationThreadConfig(serverIDs []string) map[string]any {
	config := map[string]any{
		"project_doc_max_bytes": 0,
		"web_search":            "disabled",
	}
	for _, name := range verificationDisabledFeatures {
		config["features."+name] = false
	}
	for _, id := range serverIDs {
		config["mcp_servers."+id+".enabled"] = false
	}
	return config
}

// readConfiguredServerIDs reads the MCP server ids from the app-server's
// configuration because thread/start fails on an id the configuration does not
// hold. Any failure or unexpected shape yields no ids, so an older build saves
// less and the session still starts.
func readConfiguredServerIDs(ctx context.Context, state *sessionState, logger *slog.Logger) []string {
	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()
	resp, err := state.conn.Call(callCtx, "config/read", map[string]any{
		"cwd":           state.target.WorkspacePath,
		"includeLayers": false,
	})
	if err != nil {
		logger.Debug("config/read failed, no MCP servers disabled", slog.Any("error", err))
		return nil
	}
	if resp.Error != nil {
		logger.Debug("config/read refused, no MCP servers disabled",
			slog.Int("code", resp.Error.Code), slog.String("message", resp.Error.Message))
		return nil
	}

	var result struct {
		Config struct {
			MCPServers map[string]json.RawMessage `json:"mcp_servers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		logger.Debug("config/read result not decoded, no MCP servers disabled", slog.Any("error", err))
		return nil
	}
	return slices.Sorted(maps.Keys(result.Config.MCPServers))
}

// startThread starts a thread and returns its id, plus the effective
// model and on-disk path the response reported; an omitted model or
// path is empty, never an error.
func startThread(ctx context.Context, state *sessionState, pt passthroughConfig, logger *slog.Logger) (threadID string, model string, path string, err error) {
	approvalPolicy := pt.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = "never"
	}

	sandbox := normalizeSandbox(pt.ThreadSandbox)
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	if state.credentialVerification {
		sandbox = "read-only"
	}

	params := map[string]any{
		"cwd":            state.target.WorkspacePath,
		"approvalPolicy": approvalPolicy,
		"sandbox":        sandbox,
	}
	if state.credentialVerification {
		params["ephemeral"] = true
	}
	if pt.Model != "" {
		params["model"] = pt.Model
	}
	if state.reducedVerificationContext {
		params["baseInstructions"] = agentcore.VerificationSystemPrompt
		params["config"] = verificationThreadConfig(readConfiguredServerIDs(ctx, state, logger))
	} else if pt.Personality != "" {
		params["personality"] = pt.Personality
	}

	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	resp, callErr := state.conn.Call(callCtx, "thread/start", params)
	cancel()
	if callErr != nil {
		return "", "", "", wrapConnectionLoss(fmt.Errorf("thread/start: %w", callErr))
	}
	if resp.Error != nil {
		return "", "", "", fmt.Errorf("thread/start error: code=%d message=%s", resp.Error.Code, resp.Error.Message)
	}

	var result threadResult
	if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr != nil {
		return "", "", "", fmt.Errorf("thread/start unmarshal: %w", unmarshalErr)
	}
	threadID = result.Thread.ID
	if threadID == "" {
		return "", "", "", fmt.Errorf("thread/start returned empty thread ID")
	}
	if err := awaitThreadStarted(ctx, state, logging.WithSession(logger, threadID)); err != nil {
		return "", "", "", err
	}
	return threadID, result.Model, result.path(), nil
}

// awaitThreadStarted fails only when ctx ends: some app-server versions
// never send thread/started, so the deadline and a closed stream accept
// the thread without it.
func awaitThreadStarted(ctx context.Context, state *sessionState, logger *slog.Logger) error {
	deadline := time.After(agentcore.ReadTimeout(state.agentConfig))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return nil
		case <-state.inbox.Ready():
			msg, ok := state.inbox.Take()
			if !ok {
				return nil
			}
			if msg.Kind == jsonrpc.KindStreamEnd {
				logger.Debug("scanner error waiting for thread/started", slog.Any("error", msg.Err))
				return nil
			}
			if msg.Kind == jsonrpc.KindMalformed {
				continue
			}
			if msg.Kind == jsonrpc.KindNotification && msg.Method == "thread/started" {
				return nil
			}
			handleOutOfTurnMessage(state, msg, logger)
		}
	}
}

// errResumeRejected marks resumeThread's error, for errors.Is, when the
// app-server answered thread/resume with an error response.
var errResumeRejected = errors.New("thread/resume error")

// resumeThread resumes threadID and returns the effective model and
// on-disk path the response reported, both empty when its result does
// not decode: that does not fail a successful resume. An error response
// wraps errResumeRejected, a lost connection errConnectionLost, and an
// elapsed read bound context.DeadlineExceeded.
func resumeThread(ctx context.Context, state *sessionState, threadID string) (model, path string, err error) {
	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()
	resp, callErr := state.conn.Call(callCtx, "thread/resume", map[string]any{
		"threadId": threadID,
	})
	if callErr != nil {
		return "", "", wrapConnectionLoss(fmt.Errorf("thread/resume: %w", callErr))
	}
	if resp.Error != nil {
		return "", "", fmt.Errorf("%w: code=%d message=%s", errResumeRejected, resp.Error.Code, resp.Error.Message)
	}

	var result threadResult
	if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr != nil {
		return "", "", nil
	}
	return result.Model, result.path(), nil
}

// buildSandboxPolicy returns turn/start's sandboxPolicy. The operator's
// turn_sandbox_policy may replace any key, writableRoots and
// networkAccess included.
func buildSandboxPolicy(state *sessionState, pt passthroughConfig) map[string]any {
	if state.credentialVerification {
		return map[string]any{
			"type":          "readOnly",
			"writableRoots": []string{},
			"networkAccess": false,
		}
	}

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

// normalizeSandbox maps a camelCase WORKFLOW.md sandbox value to the
// kebab-case thread/start expects; any other value passes unchanged.
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

// denormalizeSandbox maps a kebab-case sandbox value to the camelCase
// turn/start sandboxPolicy.type expects; any other value passes unchanged.
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
