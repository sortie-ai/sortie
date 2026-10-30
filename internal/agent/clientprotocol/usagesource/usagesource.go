// Package usagesource holds the per-runtime parts of this transport that the
// protocol cannot express: credential-verification additions that drop a
// runtime's agent context, and usage declarations (what a runtime's protocol
// messages report about spend, how to pull its provider usage windows when a
// turn ends, and how to read the runtime's own per-request spend records), and
// which of the runtime's prompt errors and turn notifications are provider
// usage-limit refusals or transient failures.
//
// The transport names no runtime. An addition is asked only whether it claims
// the launch.
package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Declaration is one runtime's usage declaration on the generic protocol kind,
// selected from the name the runtime gives in its handshake. A declaration that
// maps no spend of its own reads spend under Default (see Effective). Launches
// also selects the declaration the configured command names, which the
// handshake's name confirms or overrides.
type Declaration struct {
	// Live is the runtime's live source: its protocol messages and what they
	// mean. Its Category is usage.SourceProtocol. A zero Spend means the
	// declaration maps no spend of its own and reads it under Default.
	Live usage.Source

	// Pull is the runtime's pull source; its Strategy is usage.StrategyPull
	// whenever PullWindows is non-nil.
	Pull usage.Source

	// PullWindows makes one pull when a turn ends and returns the windows it
	// read. Nil when the runtime has no pull source. It returns
	// usage.ErrPullUnsupported when the runtime cannot answer in this session.
	PullWindows func(ctx context.Context, rt PullRuntime) ([]usage.WindowReading, error)

	// PromptSpend reads the spend a session/prompt result reports, under
	// Live.Spend. It returns nil when the result carries no usage, and a count
	// is nil wherever its member is absent, null, or not a JSON integer. Nil
	// exactly when Live.Spend.Shape is usage.SpendUndeclared; Effective supplies
	// Default's when the declaration maps none.
	PromptSpend func(result json.RawMessage) *usage.SpendReading

	// Records is the runtime's records source: the per-request spend records
	// on its host.
	Records usage.Source

	// ReadRecords reads every spend record of the session from the runtime's
	// host, in the order the runtime wrote them, and the refusal its records
	// hold for the session's current turn. A missing journal returns no
	// records and no error; any other failure returns an error and no
	// records. Nil exactly when Records.Spend.Shape is usage.SpendUndeclared.
	ReadRecords func(ctx context.Context, rt PullRuntime) (Records, error)

	// Launches reports whether target starts this runtime, local or remote. It
	// selects the declaration for a launch whose handshake reports no name, and
	// identifies the configured runtime a handshake name is compared against.
	// Nil when the handshake name alone selects the runtime.
	Launches func(target agentcore.LaunchTarget) bool

	// LaunchAddition turns off a model request the runtime makes on its own
	// that no source records. The zero value adds nothing.
	LaunchAddition LaunchAddition

	// UnrecordedRequests reports whether the session that a session/new,
	// session/load or session/resume result describes makes model requests no
	// source records, which leaves every turn of the session a lower bound. Nil
	// when none.
	UnrecordedRequests func(session json.RawMessage) bool

	// ChildSession reads the update object of one session/update notification
	// of session sessionID and reports what it shows of a child session: a
	// session the runtime ran part of the turn's work in. Nil when the runtime
	// runs none.
	ChildSession func(sessionID string, update json.RawMessage) ChildSignal

	// UsageUpdateBeforeRequest declares that the runtime sends a usage_update
	// session update before each model request of a turn and may write agent
	// message text of its own before the first one. Agent message text that
	// arrives before the turn's first usage_update then does not show the turn
	// reaching the model.
	UsageUpdateBeforeRequest bool

	// InitializeMeta is the clientCapabilities._meta object the initialize
	// request carries when this declaration's Launches matches the launch.
	// Nil adds nothing.
	InitializeMeta json.RawMessage

	// PromptError reads a session/prompt error response. Nil when the
	// runtime's prompt errors carry nothing the declaration reads.
	PromptError func(e jsonrpc.Error) PromptVerdict

	// TurnNotification reads a notification of session sessionID whose method
	// is neither session/update nor session/request_permission. Nil when the
	// runtime sends none the declaration reads.
	TurnNotification func(sessionID, method string, params json.RawMessage) TurnSignal

	// WindowModel returns the model the pulled window with this key meters,
	// or "" when it meters every model. Nil when no pulled window meters a
	// single model.
	WindowModel func(key string) string
}

// Refusal is a provider usage-limit refusal a declaration read off a
// session/prompt error or a notification of the turn.
type Refusal struct {
	// Name is the runtime's own name for the refusal, which its records
	// repeat. Empty when it keeps none.
	Name string

	// Message is the runtime's own text for a refusal a notification reports.
	// Empty otherwise.
	Message string

	// Limit is the model scope of the refusal. ResetsAt stays zero: the records
	// read and the pulled windows supply it.
	Limit domain.UsageLimit

	// WindowReset reports that the reset is read from the exhausted windows of
	// the turn-end pull.
	WindowReset bool
}

// PromptVerdict is what a declaration reads off one session/prompt error.
type PromptVerdict struct {
	// Refusal is non-nil when the error is a provider usage-limit refusal.
	Refusal *Refusal

	// Transient is true when the runtime classifies the error as a transient
	// provider failure.
	Transient bool
}

// TurnSignal is what one notification of the turn shows about a refusal.
type TurnSignal struct {
	// Refusal is non-nil when the notification reports a refusal.
	Refusal *Refusal

	// Retrying is true when the runtime announced a retry of a refused model
	// request of the turn.
	Retrying bool
}

// Records is what one read of a session's records returned.
type Records struct {
	// Spend holds every spend record, in the order the runtime wrote them.
	Spend []usage.SpendReading

	// Refusal is zero when the session's current turn records none.
	Refusal RecordedRefusal
}

// RecordedRefusal is the provider usage-limit refusal a session's records hold
// for its current turn.
type RecordedRefusal struct {
	// Name is the runtime's own name for the refusal. Empty when none.
	Name string

	// ResetsAt is in UTC and zero when the record carries no usable delay.
	ResetsAt time.Time
}

// LaunchAddition turns off a model request the runtime makes on its own that
// no source records, through one environment variable of every session launch.
type LaunchAddition struct {
	// Name is the variable. It is empty exactly when Merge is nil.
	Name string

	// Merge returns the value Name carries with the addition merged into
	// current, the value the launch holds for Name (set is false when none).
	// inEffect is false when current keeps the request on or cannot take the
	// addition; value is then current, unchanged.
	Merge func(current string, set bool) (value string, inEffect bool)
}

// ChildSignal is what one session update shows of a child session.
type ChildSignal struct {
	// Call is the tool call that starts or ends a child session. It is empty
	// when the update names none.
	Call string

	// Started is true when the update shows Call starting a child session.
	Started bool

	// Completed is true when the update shows Call's child session completed.
	Completed bool

	// Session is a child session identifier the update names. It is empty when
	// the update names none.
	Session string
}

// PullRuntime is what a declaration may use to reach one session's runtime. The
// transport implements it.
type PullRuntime interface {
	// SessionID returns the protocol session's ID when the pull started.
	SessionID() string

	// Request sends one request on the session's own connection and returns its
	// result. An error response whose code is the JSON-RPC method-not-found
	// code wraps ErrMethodNotFound.
	Request(ctx context.Context, method string, params any) (json.RawMessage, error)

	// LaunchEnv returns a variable as the runtime receives it from Sortie.
	LaunchEnv(name string) (string, bool)

	// ReadHomeFile reads the file path names under the runtime's home, which
	// is the home as the runtime's own host resolves it. A missing file wraps
	// fs.ErrNotExist; a path that is not valid wraps
	// agentcore.ErrInvalidRuntimePath; a file above limit bytes, a dropped
	// connection, and an ended context are errors.
	ReadHomeFile(ctx context.Context, path agentcore.RuntimePath, limit int64) ([]byte, error)

	// ScanHomeFile scans the file as ReadHomeFile locates it, calling fn with
	// each line in file order, without its terminator. The line is valid only
	// during the call. A missing file wraps fs.ErrNotExist; a path that is
	// not valid wraps agentcore.ErrInvalidRuntimePath; a file above limit
	// bytes, a line above lineLimit bytes, a remote stream that ends without
	// success, an ended context, and an error from fn stop the scan and
	// return an error.
	ScanHomeFile(ctx context.Context, path agentcore.RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error

	// MatchHomeFiles lists the home's files matching pattern, as
	// agentcore.LaunchTarget.MatchRuntimeFiles does.
	MatchHomeFiles(ctx context.Context, pattern agentcore.RuntimePath) ([]agentcore.RuntimePath, error)

	// RemoteHost reports whether the runtime runs on another host, reached
	// over SSH, rather than on Sortie's own.
	RemoteHost() bool

	// CommandWords returns the words of the configured agent command, never the
	// SSH executable.
	CommandWords() []string

	// RunHostCommand runs one command on the runtime's host, locally or
	// through the session's SSH launch, and returns its standard output. A
	// command that does not start or does not exit zero wraps
	// agentcore.ErrHostCommandFailed.
	RunHostCommand(ctx context.Context, cmd agentcore.HostCommand) ([]byte, error)

	// RunHostExchange runs one command on the runtime's host, locally or
	// through the session's SSH launch, as RunHostCommand does, and hands its
	// standard input and output to exchange.
	RunHostExchange(ctx context.Context, cmd agentcore.HostCommand, exchange func(stdin io.Writer, stdout io.Reader) error) error
}

// ErrMethodNotFound marks a request the runtime does not implement.
var ErrMethodNotFound = errors.New("method not found")

// Declarations is the registry every runtime's declaration adds itself to,
// exactly once, from an init function in its own file. The key is the
// runtime's name exactly as its handshake reports it.
//
// Registration refuses a declaration [checkDeclaration] rejects, and the key
// [DefaultName] is reserved.
var Declarations = registry.NewCheckedRegistry("usage declaration", checkDeclaration, DefaultName)

// Effective returns d with Default's spend mapping when d maps no spend of its
// own, that is, when its live source declares neither a token shape nor a
// vendor unit. Every other field is kept.
func (d Declaration) Effective() Declaration {
	if d.Live.Spend.Shape != usage.SpendUndeclared || d.Live.Spend.VendorUnit != "" {
		return d
	}
	d.Live.Spend = Default.Live.Spend
	d.PromptSpend = Default.PromptSpend
	return d
}

// checkDeclaration refuses a declaration the transport cannot measure a
// session with: the transport reads spend only off the prompt result, through
// the ledger. It also refuses an InitializeMeta that no launch selects or that
// is not a JSON object.
func checkDeclaration(d Declaration, _ struct{}) error {
	effective := d.Effective()
	if err := (usage.Sources{Live: effective.Live, Records: d.Records}).Check(); err != nil {
		return err
	}
	switch {
	case (d.LaunchAddition.Name == "") != (d.LaunchAddition.Merge == nil):
		return errors.New("LaunchAddition must set Name and Merge together")
	case d.Live.Category != usage.SourceProtocol:
		return fmt.Errorf("live source category %q is not %q", d.Live.Category, usage.SourceProtocol)
	case (d.PromptSpend == nil) != (d.Live.Spend.Shape == usage.SpendUndeclared):
		return errors.New("PromptSpend must be set exactly when the live source declares a spend shape")
	case (d.ReadRecords == nil) != (d.Records.Spend.Shape == usage.SpendUndeclared):
		return errors.New("ReadRecords must be set exactly when the records source declares a spend shape")
	case d.InitializeMeta != nil && d.Launches == nil:
		return errors.New("InitializeMeta requires Launches")
	case d.InitializeMeta != nil && !isJSONObject(d.InitializeMeta):
		return errors.New("InitializeMeta must be a JSON object")
	case d.Live.Spend.DuringTurn:
		return errors.New("live source reports during the turn, but the transport reads spend off the prompt result only")
	case effective.Live.Spend.Shape != usage.SpendUndeclared:
		if _, err := usage.NewLedger(effective.Live, d.Records); err != nil {
			return err
		}
	}
	return nil
}

func isJSONObject(raw []byte) bool {
	_, ok := jsonObject(raw)
	return ok
}

// VerificationAddition is one protocol runtime's verification-context
// switches. The transport holds values of it and names no runtime.
//
// An addition is used from the session's own goroutine only: Claim at start,
// Close at teardown.
type VerificationAddition interface {
	// Claim reports whether the addition applies to target and, when it does,
	// creates the file its switches name and returns them. It refuses a remote
	// launch, whose host and command line are not Sortie's to extend, and a
	// command line that does not name its runtime; a refusal creates nothing.
	Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool)

	// Context reports which agent context components the verification request
	// still carries when the switches apply.
	Context() registry.VerificationContext

	// Close removes what Claim created. It is safe to call on an addition that
	// never claimed and more than once.
	Close()
}

// VerificationSwitches are the launch changes one addition makes.
type VerificationSwitches struct {
	// Env holds "NAME=value" entries appended to the launch environment.
	Env []string

	// Args are appended after the operator's command line.
	Args []string
}

// VerificationAdditions is the registry every addition adds itself to,
// exactly once, from an init function in its own file. The key is the name of
// the runtime the addition serves.
var VerificationAdditions = registry.NewRegistry[func() VerificationAddition, struct{}]("verification addition")

// executableSuffixes are the platform-specific extensions a launcher may
// append to an executable name; namesRuntime strips at most one.
var executableSuffixes = []string{".cmd", ".ps1", ".exe", ".js"}

// namesRuntime reports whether the tokens of a local launch's command line name
// the executable or one of the npm packages. A remote launch names nothing here:
// verification additions claim local launches only.
func namesRuntime(target agentcore.LaunchTarget, executable string, packages ...string) bool {
	return target.RemoteCommand == "" && commandNamesRuntime(target, executable, packages...)
}

// commandNamesRuntime reports whether the configured agent command names the
// executable or one of the npm packages. A remote launch scans the
// whitespace-separated words of RemoteCommand, a string the remote shell parses,
// and never Command, which is the SSH executable. A wrong match on a remote
// launch costs one refused request pair before a puller stops.
func commandNamesRuntime(target agentcore.LaunchTarget, executable string, packages ...string) bool {
	return runtimeWordIndex(target.CommandWords(), executable, packages...) >= 0
}

// runtimeCommand cuts the configured agent command after its runtime word,
// keeping a launcher, env assignments, or a path before it and dropping what
// follows, then appends args, so the command runs the binary and environment
// the session runs. A command that names no runtime falls back to the
// executable on PATH.
func runtimeCommand(words, args []string, executable string, packages ...string) agentcore.HostCommand {
	cmd := agentcore.HostCommand{Args: args, InSession: true}
	if i := runtimeWordIndex(words, executable, packages...); i >= 0 {
		cmd.LaunchWords = i + 1
	} else {
		cmd.Name = executable
	}
	return cmd
}

// runtimeWordIndex returns the index of the first word that names the
// executable or one of the npm packages, or -1 when none does.
func runtimeWordIndex(words []string, executable string, packages ...string) int {
	for i, word := range words {
		var optionValue bool
		if i > 0 {
			prev := words[i-1]
			optionValue = strings.HasPrefix(prev, "-") && prev != "--"
		}
		if runtimeToken(word, optionValue, executable, packages) {
			return i
		}
	}
	return -1
}

func runtimeToken(token string, optionValue bool, executable string, packages []string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(token, `\`, "/"))
	if executableToken(normalized, optionValue, executable) {
		return true
	}
	for _, pkg := range packages {
		if normalized == pkg || strings.HasPrefix(normalized, pkg+"@") {
			return true
		}
		if strings.Contains(normalized, "/"+pkg+"/") || strings.HasSuffix(normalized, "/"+pkg) {
			return true
		}
	}
	return false
}

// executableToken reports whether a normalized token names the executable
// itself, bare or as a path. A bare option value with no path separator is
// ambiguous with an unrelated flag argument and is not matched.
func executableToken(normalized string, optionValue bool, executable string) bool {
	hasPathSeparator := strings.Contains(normalized, "/")
	if !hasPathSeparator && optionValue {
		return false
	}

	base := normalized
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		base = normalized[idx+1:]
	}
	for _, suffix := range executableSuffixes {
		if trimmed, ok := strings.CutSuffix(base, suffix); ok {
			base = trimmed
			break
		}
	}
	return base == executable
}
