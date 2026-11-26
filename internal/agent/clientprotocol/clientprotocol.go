// Package clientprotocol implements [domain.AgentAdapter] for the Agent Client
// Protocol, a newline-delimited JSON-RPC protocol any conforming runtime can
// speak. It launches the runtime named by agent.command as a persistent local
// subprocess, or over SSH, and drives it through a session that persists across
// turns, each turn a single open request that ends with a declared stop reason.
package clientprotocol

import (
	"context"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	// kindName repeats the registered kind because the registration keeps its
	// own literal, which the kind-completeness scan reads syntactically.
	kindName     = "agent-client-protocol"
	mcpConfigKey = "mcp_config"
	variantKey   = "variant"
)

func init() {
	registry.Agents.RegisterWithMeta("agent-client-protocol", NewClientProtocolAdapter, registry.AgentMeta{
		RequiresCommand:       true,
		ValidateAgentConfig:   validateConfig,
		MCPDialect:            registry.MCPDialectACPSession,
		LiteralSettings:       []string{envKey},
		UsageSources:          usage.Sources{Live: usagesource.Default.Live},
		CredentialEnv:         registry.DeclareCredentialEnv(),
		EffortForwarding:      registry.EffortForwarded,
		UsageLimitRecognition: registry.UsageLimitRecognized,
		VerificationContext: registry.DeclareVerificationContext(
			registry.VerificationKeepsAllTools,
			registry.VerificationKeepsProjectInstructions,
			registry.VerificationKeepsUserInstructions,
			registry.VerificationKeepsSystemPrompt,
		),
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

	// declarations is the registry finishSession selects a usage declaration
	// from. Nil means usagesource.Declarations. Set by a test before
	// StartSession, as drainGrace is.
	declarations *registry.Registry[usagesource.Declaration, struct{}]
}

// NewClientProtocolAdapter constructs a [ClientProtocolAdapter]; the kind's
// settings arrive per session, config_options among them. The env values load
// as written, because a runtime's own configuration document carries "$"
// characters that variable resolution would corrupt.
func NewClientProtocolAdapter() (domain.AgentAdapter, error) {
	return &ClientProtocolAdapter{}, nil
}

// StartSession launches the runtime, performs the initialize handshake, and
// creates a session with session/new. The usage accumulator is built here so
// exactly one exists per session and the pump inherits it.
func (a *ClientProtocolAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	return startSession(ctx, a, params, agentcore.NewTurnEndUsage())
}

func (a *ClientProtocolAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	return runTurn(ctx, session, params)
}

func (a *ClientProtocolAdapter) StopSession(ctx context.Context, session domain.Session) error {
	return stopSession(ctx, session)
}
