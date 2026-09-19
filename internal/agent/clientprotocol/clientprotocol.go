// Package clientprotocol implements [domain.AgentAdapter] for the Agent Client
// Protocol, a newline-delimited JSON-RPC protocol any conforming runtime can
// speak. It launches the runtime named by agent.command as a persistent local
// subprocess, or over SSH, and drives it through a session that persists across
// turns, each turn a single open request that ends with a declared stop reason.
package clientprotocol

import (
	"context"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

const mcpConfigKey = "mcp_config"

func init() {
	registry.Agents.RegisterWithMeta("agent-client-protocol", NewClientProtocolAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionTranslated,
		UsageArrival:        registry.UsageArrivalNone,
		UsageAttribution:    registry.UsageAttributionNone,
		CredentialEnv:       registry.DeclareCredentialEnv(),
	})
}

var _ domain.AgentAdapter = (*ClientProtocolAdapter)(nil)

// ClientProtocolAdapter satisfies [domain.AgentAdapter] for the Agent Client
// Protocol. One instance serves every concurrent session; per-session state
// lives in [sessionState], while origins is the adapter's own mutable state,
// shared by every session and outliving any one of them.
type ClientProtocolAdapter struct {
	origins sessionOrigins

	// drainGrace bounds the post-reap release's wait for the connection's own
	// reader to end normally. A non-positive value resolves to
	// procutil.DefaultDrainGrace. Set by a test before StartSession; every
	// production caller reaches only NewClientProtocolAdapter, which leaves it
	// at its zero value.
	drainGrace time.Duration
}

// NewClientProtocolAdapter constructs a [ClientProtocolAdapter] from the kind's
// configuration block. It reads mcp_config without keeping its value: the path
// a session uses arrives per session through StartSessionParams.MCPConfigPath.
// It refuses construction only when mcp_config is present with the wrong YAML
// type.
func NewClientProtocolAdapter(config map[string]any) (domain.AgentAdapter, error) {
	if _, fault := typeutil.StringField(config, mcpConfigKey); fault != nil {
		return nil, fault
	}
	return &ClientProtocolAdapter{}, nil
}

// StartSession launches the runtime, performs the initialize handshake, and
// creates a session with session/new. The usage accumulator is built here so
// exactly one exists per session and the pump inherits it.
func (a *ClientProtocolAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	return startSession(ctx, a, params)
}

func (a *ClientProtocolAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	return runTurn(ctx, session, params)
}

func (a *ClientProtocolAdapter) StopSession(ctx context.Context, session domain.Session) error {
	return stopSession(ctx, session)
}
