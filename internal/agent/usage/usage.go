// Package usage normalizes and merges the usage a runtime reports into
// runtime-neutral form. It names no runtime and no runtime format, and it
// registers no kind: each adapter declares its sources and parses its own
// reports, then hands the readings here. The component runs every pull, so
// a pulled source supplies only the request that fetches its readings. It
// also accounts a session's spend through Ledger, from the live and
// records sources a runtime declares.
package usage

import (
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// Strategy is how a source's reports reach the adapter.
type Strategy int

const (
	// StrategyLive reads the runtime's own output or protocol messages as
	// they arrive. It is the zero value.
	StrategyLive Strategy = iota
	// StrategyPull makes a request when a turn ends, to the runtime or the
	// endpoint it calls, with the runtime's own credential.
	StrategyPull
	// StrategyRecords reads the runtime's own records on its host when a turn ends.
	StrategyRecords
)

// ErrPullUnsupported marks a pull the runtime cannot answer in this
// session.
var ErrPullUnsupported = errors.New("usage pull unsupported")

// Source is one channel through which a runtime reports usage, carrying
// the declaration of what its reports mean. Each adapter defines its
// sources beside the parser that reads them.
type Source struct {
	// Strategy is how the source's reports reach the adapter.
	Strategy Strategy

	// Windows declares how the source reports provider usage windows.
	Windows WindowDeclaration

	// Spend declares how the source reports spend; the zero value leaves it
	// unread.
	Spend SpendDeclaration
}

// WindowDeclaration is a source's declaration of its usage-window fields.
type WindowDeclaration struct {
	// Share is the unit of the share, used or remaining per ShareRemaining;
	// ShareUndeclared leaves every share unread.
	Share ShareUnit

	// Statuses maps a runtime status word to a neutral status; nil leaves
	// every status unread.
	Statuses map[string]domain.UsageWindowStatus

	// Spans maps a runtime window identifier to a neutral span; an absent
	// identifier takes the reading's own span.
	Spans map[string]Span

	// ShareRemaining declares that a share counts what is left of the
	// window, not what is used.
	ShareRemaining bool

	// Units maps a runtime window identifier to the plural display name of
	// its amount's vendor unit; an absent identifier leaves its amount
	// unread.
	Units map[string]string
}

// ShareUnit is the unit in which a source reports a share.
type ShareUnit int

const (
	// ShareUndeclared leaves every share unread.
	ShareUndeclared ShareUnit = iota
	// ShareFraction reports 1 as the whole window.
	ShareFraction
	// SharePercent reports 100 as the whole window.
	SharePercent
)

// Span is a window's length and the usage it covers, in vendor-neutral
// terms.
type Span struct {
	// Minutes is the window length; 0 when not stated.
	Minutes int64

	// Period states the window length in words when Minutes cannot, such
	// as "Monthly"; read only when Minutes is 0.
	Period string

	// Scope is the model or usage pool the window covers, as display text;
	// empty for all of the account's usage.
	Scope string
}

// WindowReading is one usage window as a source's parser read it, in the
// runtime's own terms.
type WindowReading struct {
	// Window is the runtime's identifier of the window; may be empty.
	Window string

	// Span is the length and scope the reading itself states; zero when it
	// states none.
	Span Span

	// Share is the used share in the source's declared unit; nil when
	// absent.
	Share *float64

	// Status is the runtime's status word; empty when absent.
	Status string

	// ResetsAt is the reset instant in Unix seconds; zero or negative when
	// absent.
	ResetsAt int64

	// Used is the amount used, in the unit the declaration maps for
	// Window; nil when absent.
	Used *float64

	// Limit is the window's limit in the same unit; nil when absent.
	Limit *float64

	// ResetsAfter is the reset as a delay after the observation; read only
	// when ResetsAt is absent.
	ResetsAfter time.Duration

	// ObservedAt is the Unix second the runtime itself took the reading;
	// zero when it states none.
	ObservedAt int64
}
