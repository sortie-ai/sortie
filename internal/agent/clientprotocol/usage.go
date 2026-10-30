package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/registry"
)

// declarationChoice is the declaration a session is measured under and what
// selected it.
type declarationChoice struct {
	// declaration is the effective declaration: spend mapped, by Default when
	// the selected one maps none.
	declaration usagesource.Declaration

	// key is the registry key of the selected declaration, or
	// usagesource.DefaultName.
	key string

	// configured is the key of the declaration the configured command names,
	// empty when none does.
	configured string

	// mismatch is true when the configured command names a runtime the
	// handshake does not confirm.
	mismatch bool
}

// chooseDeclaration selects the declaration a session is measured under. The
// runtime the handshake names wins; a handshake naming a runtime with no
// declaration selects usagesource.Default, and one naming nothing keeps the
// declaration the configured command names. It claims nothing, so a remote
// launch is chosen like a local one.
func chooseDeclaration(declarations *registry.Registry[usagesource.Declaration, struct{}], info *implementation, target agentcore.LaunchTarget) declarationChoice {
	named := ""
	if info != nil {
		named = info.Name
	}
	launches := func(kind string) bool {
		declaration, err := declarations.Get(kind)
		return err == nil && declaration.Launches != nil && declaration.Launches(target)
	}

	configured := ""
	for _, kind := range declarations.Kinds() {
		if launches(kind) {
			configured = kind
			break
		}
	}

	var key string
	switch {
	case named != "" && declarations.Has(named):
		key = named
	case named != "":
		key = usagesource.DefaultName
	case configured != "":
		key = configured
	default:
		key = usagesource.DefaultName
	}

	declaration := usagesource.Default
	if key != usagesource.DefaultName {
		declaration, _ = declarations.Get(key) // a key chosen from the registry resolves
	}

	confirmed := named == "" || (declarations.Has(named) && launches(named))
	return declarationChoice{
		declaration: declaration.Effective(),
		key:         key,
		configured:  configured,
		mismatch:    configured != "" && !confirmed,
	}
}

// pullRuntime is a declaration's only path to the session's connection, launch
// environment, and runtime home.
type pullRuntime struct {
	state *sessionState

	// session names a child session whose records are read; empty means the
	// session's own.
	session string
}

func (r pullRuntime) SessionID() string {
	if r.session != "" {
		return r.session
	}
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

func (r pullRuntime) ReadHomeFile(ctx context.Context, path agentcore.RuntimePath, limit int64) ([]byte, error) {
	return r.state.target.ReadRuntimeFile(ctx, r.state.launchEnv, path, limit)
}

func (r pullRuntime) ScanHomeFile(ctx context.Context, path agentcore.RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error {
	return r.state.target.ScanRuntimeFile(ctx, r.state.launchEnv, path, limit, lineLimit, fn)
}

func (r pullRuntime) MatchHomeFiles(ctx context.Context, pattern agentcore.RuntimePath) ([]agentcore.RuntimePath, error) {
	return r.state.target.MatchRuntimeFiles(ctx, r.state.launchEnv, pattern)
}

func (r pullRuntime) RemoteHost() bool {
	return r.state.target.RemoteCommand != ""
}

func (r pullRuntime) CommandWords() []string {
	return r.state.target.CommandWords()
}

func (r pullRuntime) RunHostCommand(ctx context.Context, cmd agentcore.HostCommand) ([]byte, error) {
	if addition := r.state.launchAddition; addition.set {
		cmd.Settings = []sshutil.EnvVar{{Name: addition.name, Value: addition.value}}
	}
	return r.state.target.RunHostCommand(ctx, r.state.launchEnv, cmd)
}

func (r pullRuntime) RunHostExchange(ctx context.Context, cmd agentcore.HostCommand, exchange func(stdin io.Writer, stdout io.Reader) error) error {
	if addition := r.state.launchAddition; addition.set {
		cmd.Settings = []sshutil.EnvVar{{Name: addition.name, Value: addition.value}}
	}
	return r.state.target.RunHostExchange(ctx, r.state.launchEnv, cmd, exchange)
}

// newSessionPuller builds the session's puller from decl, or returns nil when
// the declaration has no pull or the usage component declines the session.
func newSessionPuller(decl usagesource.Declaration, state *sessionState, credentialVerification bool) *usage.Puller {
	if decl.PullWindows == nil {
		return nil
	}
	runtime := pullRuntime{state: state}
	return usage.NewPuller(decl.Pull, agentcore.AuxiliaryTimeout(state.agentConfig), credentialVerification, func(ctx context.Context) ([]usage.WindowReading, error) {
		return decl.PullWindows(ctx, runtime)
	})
}

// readSessionRecords reads every spend record of the session from the runtime's
// host under the read timeout.
func readSessionRecords(ctx context.Context, state *sessionState, decl usagesource.Declaration) usage.RecordsRead {
	read, _ := readTurnRecords(ctx, state, decl, nil)
	return read
}

// readTurnRecords reads the session's records and then those of each child
// session, in the order given, under one read timeout. A failed child read
// leaves the later children read. ChildrenOK is false for a read naming no
// child. The refusal returned is the session's own, zero when that read failed;
// a child's refusal is not this turn's.
func readTurnRecords(ctx context.Context, state *sessionState, decl usagesource.Declaration, children []string) (usage.RecordsRead, usagesource.RecordedRefusal) {
	readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()

	var read usage.RecordsRead
	own, ok := readRecordsOf(readCtx, state, decl, "")
	read.Readings, read.OK = own.Spend, ok
	if !ok {
		own.Refusal = usagesource.RecordedRefusal{}
	}
	childrenOK := len(children) > 0
	for _, child := range children {
		records, ok := readRecordsOf(readCtx, state, decl, child)
		read.ChildReadings = append(read.ChildReadings, records.Spend...)
		childrenOK = childrenOK && ok
	}
	read.ChildrenOK = childrenOK
	return read, own.Refusal
}

// readRecordsOf reads the records of the child session named, or of the
// session's own when child is empty, and logs a failed read.
func readRecordsOf(ctx context.Context, state *sessionState, decl usagesource.Declaration, child string) (usagesource.Records, bool) {
	records, err := decl.ReadRecords(ctx, pullRuntime{state: state, session: child})
	if err != nil {
		state.logger.Debug("usage records read failed", slog.Any("error", err))
	}
	return records, err == nil
}

// closeVerification closes the verification addition, once teardown has ended
// the runtime.
func closeVerification(state *sessionState) {
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
