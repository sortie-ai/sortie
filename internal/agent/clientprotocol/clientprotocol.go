// Package clientprotocol implements [domain.AgentAdapter] for the Agent
// Client Protocol, a generic newline-delimited JSON-RPC protocol any
// conforming runtime can speak. It launches the runtime named by
// agent.command as a persistent local subprocess, or over SSH, and
// drives it through a session that persists across turns, each turn a
// single open request that ends with a declared stop reason.
package clientprotocol

import (
	"context"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// mcpConfigKey is the one settings-block key this adapter reads.
const mcpConfigKey = "mcp_config"

func init() {
	registry.Agents.RegisterWithMeta("agent-client-protocol", NewClientProtocolAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionTranslated,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*ClientProtocolAdapter)(nil)

// ClientProtocolAdapter satisfies [domain.AgentAdapter] for the Agent
// Client Protocol. One adapter instance serves every concurrent session;
// it holds no mutable state of its own, and all per-session state lives
// in [sessionState], reached through [domain.Session.Internal].
type ClientProtocolAdapter struct{}

// NewClientProtocolAdapter constructs a [ClientProtocolAdapter] from the
// kind's own configuration block. It reads exactly one key, mcp_config,
// without keeping its value: the path an agent's session actually uses
// arrives per session through StartSessionParams.MCPConfigPath rather
// than through this block. It succeeds when the block is absent or
// empty; it refuses construction only when mcp_config is present with
// the wrong YAML type.
func NewClientProtocolAdapter(config map[string]any) (domain.AgentAdapter, error) {
	if _, fault := typeutil.StringField(config, mcpConfigKey); fault != nil {
		return nil, fault
	}
	return &ClientProtocolAdapter{}, nil
}

// StartSession launches the runtime, performs the initialize handshake,
// and creates a session with session/new.
func (a *ClientProtocolAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	return startSession(ctx, params)
}

// RunTurn runs one prompt turn on an existing session.
func (a *ClientProtocolAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	return runTurn(ctx, session, params)
}

// StopSession tears down the session's subprocess and connection.
func (a *ClientProtocolAdapter) StopSession(ctx context.Context, session domain.Session) error {
	return stopSession(ctx, session)
}

// EventStream returns nil. This adapter delivers every event
// synchronously through RunTurn's OnEvent callback, matching every
// other registered kind.
func (a *ClientProtocolAdapter) EventStream() <-chan domain.AgentEvent { return nil }
