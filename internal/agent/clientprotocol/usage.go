package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/registry"
)

// usageReader supplies token measurements read outside the wire, for a runtime
// without a spend declaration. A reader that fails to claim the launch target,
// recognize the handshake's runtime name, or produce a record on its first
// drain is dropped, leaving the session unmeasured and the token ceiling
// inactive.
//
// Drain reports whether a measurement exists; Completeness grades how far the
// figure it just returned was proven to reach.
type usageReader interface {
	Claim(target agentcore.LaunchTarget, runtime string) ([]string, bool)
	Recognize(name string) bool
	Open(sessionID string)
	Drain(ctx context.Context, lowerBound int64) (agentcore.RecoveredUsage, string, bool)
	Completeness() usagesource.Completeness
	Close()
}

type usageClaim struct {
	source      usageReader
	assignments []string
}

// usageClaims is one offer round's outcome, in registry order.
type usageClaims struct {
	claimed   []usageClaim
	unclaimed []usageReader
}

// usageVerdict is one start's handshake settlement of a usageClaims. recognizer
// is set only when the first start confirmed no claimed source.
type usageVerdict struct {
	confirmed  *usageClaim
	released   []usageReader
	recognizer usageReader
}

// claimUsageSources offers target, with an empty runtime name, to a fresh
// instance of every registered source and buckets each by whether it accepts.
func claimUsageSources(sources *registry.Registry[usagesource.Constructor, struct{}], target agentcore.LaunchTarget) usageClaims {
	var claims usageClaims
	for _, kind := range sources.Kinds() {
		constructor, _ := sources.Get(kind) // a listed kind always resolves
		source := constructor()
		if assignments, ok := source.Claim(target, ""); ok {
			claims.claimed = append(claims.claimed, usageClaim{source: source, assignments: assignments})
			continue
		}
		claims.unclaimed = append(claims.unclaimed, source)
	}
	return claims
}

// settleUsageSources settles claims against one start's handshake. The first
// claimed source that recognizes the reported name is confirmed and the rest are
// released. Only when none is confirmed, the name is non-empty, and the launch is
// local does the first unclaimed source recognizing it become the recognizer.
func settleUsageSources(claims usageClaims, info *implementation, remote bool) usageVerdict {
	name := ""
	if info != nil {
		name = info.Name
	}

	confirmedIndex := -1
	if name != "" {
		for i := range claims.claimed {
			if claims.claimed[i].source.Recognize(name) {
				confirmedIndex = i
				break
			}
		}
	}

	var verdict usageVerdict
	for i := range claims.claimed {
		if i == confirmedIndex {
			verdict.confirmed = &claims.claimed[i]
			continue
		}
		verdict.released = append(verdict.released, claims.claimed[i].source)
	}
	if verdict.confirmed == nil && name != "" && !remote {
		for _, source := range claims.unclaimed {
			if source.Recognize(name) {
				verdict.recognizer = source
				break
			}
		}
	}
	return verdict
}

// selectDeclaration returns the declaration registered under the handshake's
// name, or the zero Declaration. It claims nothing, so a remote launch is
// selected like a local one.
func selectDeclaration(declarations *registry.Registry[usagesource.Declaration, struct{}], info *implementation) usagesource.Declaration {
	if info == nil || info.Name == "" {
		return usagesource.Declaration{}
	}
	declaration, err := declarations.Get(info.Name)
	if err != nil {
		return usagesource.Declaration{}
	}
	return declaration
}

// selectDeclarationByLaunch returns the first declaration whose Launches accepts
// target, for a runtime whose handshake names no registered declaration.
func selectDeclarationByLaunch(declarations *registry.Registry[usagesource.Declaration, struct{}], target agentcore.LaunchTarget) usagesource.Declaration {
	for _, kind := range declarations.Kinds() {
		declaration, err := declarations.Get(kind)
		if err == nil && declaration.Launches != nil && declaration.Launches(target) {
			return declaration
		}
	}
	return usagesource.Declaration{}
}

// resolveDeclaration returns the declaration registered under the handshake's
// name, else the one that claims the launch target.
func resolveDeclaration(declarations *registry.Registry[usagesource.Declaration, struct{}], info *implementation, target agentcore.LaunchTarget) usagesource.Declaration {
	if info != nil && declarations.Has(info.Name) {
		return selectDeclaration(declarations, info)
	}
	return selectDeclarationByLaunch(declarations, target)
}

// pullRuntime is a declaration's only path to the session's connection, launch
// environment, and runtime home.
type pullRuntime struct {
	state *sessionState
}

func (r pullRuntime) SessionID() string {
	return r.state.sessionID
}

func (r pullRuntime) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	resp, err := r.state.conn.Call(ctx, method, params)
	if err != nil {
		return nil, fmt.Errorf("pull request %s: %w", method, err)
	}
	if resp.Error == nil {
		return resp.Result, nil
	}
	if resp.Error.Code == jsonrpc.MethodNotFoundCode {
		return nil, fmt.Errorf("pull request %s: %w", method, usagesource.ErrMethodNotFound)
	}
	return nil, fmt.Errorf("pull request %s: error %d: %s", method, resp.Error.Code, quoteJSONRPCError(resp.Error))
}

func (r pullRuntime) LaunchEnv(name string) (string, bool) {
	return r.state.target.RuntimeEnv(r.state.launchEnv, name)
}

func (r pullRuntime) ReadHomeFile(ctx context.Context, homeVar, rel string, limit int64) ([]byte, error) {
	return r.state.target.ReadRuntimeFile(ctx, r.state.launchEnv, homeVar, rel, limit)
}

func (r pullRuntime) ScanHomeFile(ctx context.Context, homeVar, rel string, limit, lineLimit int64, fn func(line []byte) error) error {
	return r.state.target.ScanRuntimeFile(ctx, r.state.launchEnv, homeVar, rel, limit, lineLimit, fn)
}

func (r pullRuntime) RemoteHost() bool {
	return r.state.target.RemoteCommand != ""
}

func (r pullRuntime) RunHostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.state.target.RunHostCommand(ctx, name, args...)
}

// newSessionPuller builds the session's puller from decl, or returns nil when
// the declaration has no pull or the session verifies a credential.
func newSessionPuller(decl usagesource.Declaration, state *sessionState, credentialVerification bool) *usage.Puller {
	if decl.PullWindows == nil || credentialVerification {
		return nil
	}
	runtime := pullRuntime{state: state}
	return usage.NewPuller(decl.Pull, agentcore.AuxiliaryTimeout(state.agentConfig), func(ctx context.Context) ([]usage.WindowReading, error) {
		return decl.PullWindows(ctx, runtime)
	})
}

// releaseUsageSources closes every claimed source and the verification addition.
// It runs after teardown, once no drain is still reading what a source removes.
func releaseUsageSources(state *sessionState) {
	for _, source := range state.claimed {
		source.Close()
	}
	if state.verification != nil {
		state.verification.Close()
	}
}

// promptQuota is the wire extension a prompt result may carry. It is a
// presence signal and a lower bound, never the accounting figure: it omits
// cache-read and reasoning tokens, and names the serving model wrongly often
// enough that its per-model breakdown is not read here at all.
type promptQuota struct {
	Quota *struct {
		TokenCount struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"token_count"`
	} `json:"quota"`
}

// spendLowerBound returns the input-plus-output figure a prompt result reports
// for its own turn, and whether the extension was present at all. A present
// block reporting zero is a turn that reached no model, which is why presence
// is reported separately from the bound.
func spendLowerBound(meta json.RawMessage) (int64, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	var decoded promptQuota
	if err := json.Unmarshal(meta, &decoded); err != nil || decoded.Quota == nil {
		return 0, false
	}
	count := decoded.Quota.TokenCount
	return max(count.InputTokens, 0) + max(count.OutputTokens, 0), true
}
