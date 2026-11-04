package storage

import (
	"context"
	"time"
)

// RunEvent is one stored event of a finished run. Payload is the event's
// members other than Seq, At, and Event, as a JSON object; Size is the byte
// length of At, Event, and Payload together.
type RunEvent struct {
	Seq     int64
	At      string // RFC 3339 with nanoseconds, UTC.
	Event   string
	Payload string
	Size    int64
}

// RunEventLimits bounds the stored events of every run together.
type RunEventLimits struct {
	// MaxAge is the least a run's events stay stored, counted from the
	// completion of the run whose write prunes them.
	MaxAge time.Duration

	// MaxBytes caps the sum of the stored events' Size.
	MaxBytes int64

	// MaxExpiredRuns is the most runs past MaxAge one write deletes.
	MaxExpiredRuns int
}

// RunEventsError reports that a run's history row was stored and its events
// were not.
type RunEventsError struct {
	Err error
}

// Error returns the message of the underlying error.
func (e *RunEventsError) Error() string {
	return "store run events: " + e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *RunEventsError) Unwrap() error {
	return e.Err
}

// RunEventStore records the events of finished runs.
type RunEventStore interface {
	// AppendRunHistoryWithEvents stores run, with its model usage, reported
	// amounts, and turn records, and its events in one write, then prunes the
	// stored events under limits; a crash leaves the run and its events both
	// stored or neither. A *[RunEventsError] means run, its parts, its amounts,
	// and its turn records were stored and events were not; any other error
	// means none of them was. [RunHistoryStore.AppendRunHistory] writes no turn
	// records.
	AppendRunHistoryWithEvents(ctx context.Context, run RunHistory, events []RunEvent, limits RunEventLimits) (RunHistory, error)
	AppendRunEvents(ctx context.Context, dispatchID, completedAt string, events []RunEvent, limits RunEventLimits) error
	RunEvents(ctx context.Context, dispatchID string, newest int) ([]RunEvent, error)
}
