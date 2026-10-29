package usage

import (
	"errors"
	"fmt"
	"reflect"
)

// Sources is what one agent kind, or one runtime declaration under the
// generic protocol kind, attaches to every session it runs, local or remote.
type Sources struct {
	// Live is the runtime's own output or its protocol messages. Required.
	Live Source

	// Records is the runtime's own session records; the zero Source when no
	// records source is attached.
	Records Source
}

// Arrival is when a session's token figure reaches the orchestrator. The
// empty value means undeclared.
type Arrival string

const (
	// ArrivalUndeclared is the zero value.
	ArrivalUndeclared Arrival = ""

	// ArrivalIncremental declares that the live source reports token counts
	// after each model request while the turn runs. A session of this
	// arrival reports api_request_count as a request count once a figure
	// has arrived.
	ArrivalIncremental Arrival = "incremental"

	// ArrivalTurnEnd declares that at most one token figure settles per
	// turn, after the turn's work and before its terminal event.
	// api_request_count is not a count of API requests for such a session.
	ArrivalTurnEnd Arrival = "turn_end"

	// ArrivalNone declares that no attached source counts tokens. It is
	// derived, never declared.
	ArrivalNone Arrival = "none"
)

// Attribution is what a session's token figure names. The empty value means
// undeclared.
type Attribution string

const (
	// AttributionUndeclared is the zero value.
	AttributionUndeclared Attribution = ""

	// AttributionPerModel declares that at least one usage event of the
	// session names a model.
	AttributionPerModel Attribution = "per_model"

	// AttributionSessionTotal declares that the kind guarantees no model
	// name, though a figure may still name one when a source the session
	// attaches names it.
	AttributionSessionTotal Attribution = "session_total"

	// AttributionNone declares that there is no figure to attribute.
	AttributionNone Attribution = "none"
)

// ErrNoLiveSource marks a registration whose live source is missing or does
// not report spend.
var ErrNoLiveSource = errors.New("no live usage source")

// ReportsDuringTurn reports whether a figure of this arrival reaches the
// orchestrator while a turn is running.
func (a Arrival) ReportsDuringTurn() bool {
	return a == ArrivalIncremental
}

// ReportsAnyFigure reports whether a session of this arrival ever produces a
// usage figure.
func (a Arrival) ReportsAnyFigure() bool {
	return a == ArrivalIncremental || a == ArrivalTurnEnd
}

// NamesModel reports whether a figure of this attribution names the model
// that produced it.
func (t Attribution) NamesModel() bool {
	return t == AttributionPerModel
}

// Check returns nil when s is a valid registration, or the first defect found.
// A defect in the live source wraps [ErrNoLiveSource].
func (s Sources) Check() error {
	switch {
	case s.Live.Category != SourceProtocol && s.Live.Category != SourceOutput:
		return fmt.Errorf("live source category %q: %w", s.Live.Category, ErrNoLiveSource)
	case s.Live.Strategy != StrategyLive:
		return fmt.Errorf("live source strategy %d: %w", s.Live.Strategy, ErrNoLiveSource)
	case s.Live.Spend.Shape == SpendUndeclared && s.Live.Spend.VendorUnit == "":
		return fmt.Errorf("live source reports no spend: %w", ErrNoLiveSource)
	}
	if reflect.ValueOf(s.Records).IsZero() {
		return nil
	}
	switch {
	case s.Records.Category != SourceJournal:
		return fmt.Errorf("records source category %q is not %q", s.Records.Category, SourceJournal)
	case s.Records.Strategy != StrategyRecords:
		return fmt.Errorf("records source strategy %d is not records", s.Records.Strategy)
	case s.Records.Spend.Shape == SpendUndeclared:
		return errors.New("records source counts no tokens")
	}
	return nil
}

// Disposition derives the arrival and attribution of a session with these
// sources. It is defined for any Sources that pass [Sources.Check], and
// reads no passthrough, launch mode, or runtime identity.
func (s Sources) Disposition() (Arrival, Attribution) {
	recordsSplit := countsTokens(s.Records) && s.Records.Spend.ServedModel
	switch {
	case countsTokens(s.Live):
		arrival := ArrivalTurnEnd
		if s.Live.Spend.DuringTurn {
			arrival = ArrivalIncremental
		}
		if s.Live.Spend.ServedModel || recordsSplit {
			return arrival, AttributionPerModel
		}
		return arrival, AttributionSessionTotal
	case countsTokens(s.Records):
		if recordsSplit {
			return ArrivalTurnEnd, AttributionPerModel
		}
		return ArrivalTurnEnd, AttributionSessionTotal
	}
	return ArrivalNone, AttributionNone
}

// CarriesTokenFigure reports whether a session with these sources can carry
// a token figure on a local launch (remote false) or a remote launch.
//
// On a remote launch only the live source counts: a records source is read
// from the runtime's host with local file access, which a remote session does
// not have.
func (s Sources) CarriesTokenFigure(remote bool) bool {
	if remote {
		s.Records = Source{}
	}
	arrival, _ := s.Disposition()
	return arrival.ReportsAnyFigure()
}

func countsTokens(src Source) bool {
	return src.Spend.Shape != SpendUndeclared
}
