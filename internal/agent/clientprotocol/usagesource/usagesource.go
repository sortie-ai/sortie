// Package usagesource holds the per-runtime parts of this transport that the
// protocol cannot express: measurement sources read outside the wire,
// credential-verification additions that drop a runtime's agent context, and
// usage declarations (what a runtime's protocol messages report about provider
// usage windows and spend, how to pull those windows when a turn ends, and how
// to read the runtime's own per-request spend records).
//
// The transport names no runtime. It asks each source three questions in
// order: does it claim this launch, does the handshake report the runtime's
// name, and what has it read for this session. A no to either of the first two
// leaves the session unmeasured. An addition is asked only whether it claims
// the launch.
package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Constructor returns a fresh Reader for one session.
type Constructor func() Reader

// Sources is the registry every source adds itself to, exactly once, from an
// init function in its own file. The key is the name of the runtime the
// source reads, a name its Recognize accepts.
var Sources = registry.NewRegistry[Constructor, struct{}]("usage source")

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

	// UsageUpdateWindows reads the usage windows one usage_update session
	// update reports in its _meta object. Nil when the runtime reports none
	// there. A _meta without the runtime's member returns nil, nil.
	UsageUpdateWindows func(meta json.RawMessage) ([]usage.WindowReading, error)

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
	// host, in the order the runtime wrote them. A missing journal returns no
	// readings and no error; any other failure returns an error and no
	// readings. Nil exactly when Records.Spend.Shape is usage.SpendUndeclared.
	ReadRecords func(ctx context.Context, rt PullRuntime) ([]usage.SpendReading, error)

	// Launches reports whether target starts this runtime, local or remote. It
	// selects the declaration for a launch whose handshake reports no name, and
	// identifies the configured runtime a handshake name is compared against.
	// Nil when the handshake name alone selects the runtime.
	Launches func(target agentcore.LaunchTarget) bool
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

	// ReadHomeFile reads rel under the runtime's home on its host, which is
	// the value of homeVar in the launch environment, else the user's home
	// directory. A missing file wraps fs.ErrNotExist; a file above limit
	// bytes is an error.
	ReadHomeFile(ctx context.Context, homeVar, rel string, limit int64) ([]byte, error)

	// ScanHomeFile scans rel as ReadHomeFile locates it, calling fn with each
	// line in file order, without its terminator. The line is valid only
	// during the call. A missing file wraps fs.ErrNotExist; a file above
	// limit bytes, a line above lineLimit bytes, an ended context, and an
	// error from fn stop the scan and return an error.
	ScanHomeFile(ctx context.Context, homeVar, rel string, limit, lineLimit int64, fn func(line []byte) error) error

	// RemoteHost reports whether the runtime runs on another host, reached
	// over SSH, rather than on Sortie's own.
	RemoteHost() bool

	// RunHostCommand runs one command on the runtime's host, locally or
	// through the session's SSH launch, and returns its standard output. A
	// command that does not start or does not exit zero wraps
	// agentcore.ErrHostCommandFailed.
	RunHostCommand(ctx context.Context, name string, args ...string) ([]byte, error)
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
// the ledger.
func checkDeclaration(d Declaration, _ struct{}) error {
	effective := d.Effective()
	if err := (usage.Sources{Live: effective.Live, Records: d.Records}).Check(); err != nil {
		return err
	}
	switch {
	case d.Live.Category != usage.SourceProtocol:
		return fmt.Errorf("live source category %q is not %q", d.Live.Category, usage.SourceProtocol)
	case (d.PromptSpend == nil) != (d.Live.Spend.Shape == usage.SpendUndeclared):
		return errors.New("PromptSpend must be set exactly when the live source declares a spend shape")
	case (d.ReadRecords == nil) != (d.Records.Spend.Shape == usage.SpendUndeclared):
		return errors.New("ReadRecords must be set exactly when the records source declares a spend shape")
	case d.Live.Spend.DuringTurn:
		return errors.New("live source reports during the turn, but the transport reads spend off the prompt result only")
	case effective.Live.Spend.Shape != usage.SpendUndeclared:
		if _, err := usage.NewLedger(effective.Live, d.Records); err != nil {
			return err
		}
	}
	return nil
}

// Reader is one measurement source. The transport declares its own interface
// with this method set and holds values of it.
//
// A Reader is used from two goroutines and only in this order: Claim and Close
// on the session's own goroutine, and Recognize, Open and Drain on the message
// pump. Drain is the one blocking method and runs on a goroutine the pump
// starts, not the pump itself.
type Reader interface {
	// Claim reports whether this source can measure a session launched against
	// target and, when it can, arms itself and returns the environment
	// assignments ("NAME=value") the launch needs. It refuses every launch with
	// target.RemoteCommand set. With runtime empty (the first offer), a source
	// whose claim returns an assignment or creates a file or directory claims
	// only a local launch whose command line (target.Command and every element
	// of target.Args) names the runtime it reads; a claim that returns no
	// assignment and creates nothing may be made for any local launch. With
	// runtime non-empty (the relaunch offer, carrying the name the first
	// start's handshake reported), it claims a local launch exactly when
	// Recognize(runtime) is true. A refusal returns nil, false and creates
	// nothing.
	Claim(target agentcore.LaunchTarget, runtime string) ([]string, bool)

	// Recognize reports whether name, the runtime's own name from the
	// handshake, is the runtime this source maps. No build is refused by its
	// version.
	Recognize(name string) bool

	// Open records the session identifier whose records this source counts and
	// the position in its own output the session starts at, so a resumed
	// runtime's history contributes nothing.
	Open(sessionID string)

	// Drain returns the session's run-cumulative usage as this source measured
	// it, waiting up to its own deadline for the turn's records to become
	// readable. lowerBound is the turn's own input-plus-output figure from the
	// protocol's presence signal, letting the wait end early; a non-positive
	// lowerBound means no such signal was read. found is false when the source
	// measured nothing at all, which is not a measurement of zero.
	Drain(ctx context.Context, lowerBound int64) (usage agentcore.RecoveredUsage, source string, found bool)

	// Completeness reports how far the figure the most recent Drain returned was
	// proven to account for that turn; a figure can be a measurement and still
	// be short of the turn. It describes one Drain call and is reset by the
	// next, so it is read on the goroutine that called Drain, before another
	// begins.
	Completeness() Completeness

	// Close releases whatever Claim armed. It is safe to call on a source that
	// never claimed.
	Close()
}

// Completeness is how far a drained figure was proven to account for the turn
// it was drained for. It is not a verdict on whether a measurement exists.
type Completeness int

const (
	// CompletenessUnknown is a turn with no bound to prove a figure against,
	// which differs from proving it short.
	CompletenessUnknown Completeness = iota

	// CompletenessPartial is a figure that did not reach the turn's own bound.
	CompletenessPartial

	// CompletenessAccounted is a figure that reached the turn's own bound.
	CompletenessAccounted
)

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
// measurement sources and verification additions claim local launches only.
func namesRuntime(target agentcore.LaunchTarget, executable string, packages ...string) bool {
	return target.RemoteCommand == "" && commandNamesRuntime(target, executable, packages...)
}

// commandNamesRuntime reports whether the configured agent command names the
// executable or one of the npm packages. A remote launch scans the
// whitespace-separated words of RemoteCommand, a string the remote shell parses,
// and never Command, which is the SSH executable. A wrong match on a remote
// launch costs one refused request pair before a puller stops.
func commandNamesRuntime(target agentcore.LaunchTarget, executable string, packages ...string) bool {
	var tokens []string
	if target.RemoteCommand != "" {
		tokens = strings.Fields(target.RemoteCommand)
	} else {
		tokens = append([]string{target.Command}, target.Args...)
	}
	for i, token := range tokens {
		var optionValue bool
		if i > 0 {
			prev := tokens[i-1]
			optionValue = strings.HasPrefix(prev, "-") && prev != "--"
		}
		if runtimeToken(token, optionValue, executable, packages) {
			return true
		}
	}
	return false
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
