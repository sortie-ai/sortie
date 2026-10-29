package usage

import (
	"context"
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// Puller runs one session's pulls for a source whose Strategy is
// StrategyPull and normalizes what each returns. It is not safe for
// concurrent use: one turn calls it at a time.
type Puller struct {
	source  Source
	bound   time.Duration
	pull    func(context.Context) ([]WindowReading, error)
	stopped bool
}

// NewPuller returns one session's puller for source, or nil for a
// credential-verification session, which pulls nothing. Each Pull runs pull
// within bound.
func NewPuller(source Source, bound time.Duration, credentialVerification bool, pull func(context.Context) ([]WindowReading, error)) *Puller {
	if credentialVerification {
		return nil
	}
	return &Puller{source: source, bound: bound, pull: pull}
}

// TurnEnding is how a turn ended, as the transport that ran it observed it
// where a pull can run. The zero value is no ending.
type TurnEnding uint8

// The endings a transport reports to a Puller.
const (
	// EndingAnswered is a turn the runtime ended itself, whatever the
	// outcome: its prompt response arrived, or its process exited on its own.
	EndingAnswered TurnEnding = iota + 1
	// EndingTimedOut is a turn the adapter stopped because no first event
	// arrived in time.
	EndingTimedOut
	// EndingReadFailed is a turn the adapter stopped because it could not
	// read the runtime's output.
	EndingReadFailed
	// EndingCancelled is a turn Sortie cancelled.
	EndingCancelled
	// EndingHumanInput is a turn the adapter ended by cancelling the
	// runtime's prompt because the runtime asked for a person.
	EndingHumanInput
	// EndingProtocolBroken is a turn the adapter ended because the
	// runtime's output broke the protocol.
	EndingProtocolBroken
)

// PullsAfter reports whether a turn that ended so runs a pull. It is false
// for a nil Puller and for an ending the policy excludes.
func (p *Puller) PullsAfter(ending TurnEnding) bool {
	if p == nil {
		return false
	}
	return ending == EndingAnswered
}

// Pull runs one pull after a turn that ended so. It returns nil, nil without
// calling pull when PullsAfter is false, when ctx is done, or once an earlier
// pull returned an error wrapping ErrPullUnsupported. Otherwise it returns
// the readings normalized by Windows, observed when pull returned, or nil
// when there are none. The error is pull's.
func (p *Puller) Pull(ctx context.Context, ending TurnEnding) ([]domain.UsageWindow, error) {
	if !p.PullsAfter(ending) || p.stopped || ctx.Err() != nil {
		return nil, nil
	}
	pullCtx, cancel := context.WithTimeout(ctx, p.bound)
	defer cancel()
	readings, err := p.pull(pullCtx)
	at := time.Now().UTC()
	if errors.Is(err, ErrPullUnsupported) {
		p.stopped = true
	}
	if err != nil {
		return nil, err
	}
	return Windows(p.source, readings, at), nil
}
