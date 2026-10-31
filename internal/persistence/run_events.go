package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

const runEventsSavepoint = "run_events"

// AppendRunHistoryWithEvents appends run, its [RunHistory.ModelUsage] with
// the [RunHistory.ModelCosts] pricing it, its [RunHistory.ReportedAmounts],
// and events under *run.DispatchID in one transaction, then prunes under
// limits. A *[RunEventsError] means the
// row was stored and the events were not; any other error means neither was.
func (s *Store) AppendRunHistoryWithEvents(ctx context.Context, run RunHistory, events []RunEvent, limits RunEventLimits) (RunHistory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunHistory{}, fmt.Errorf("begin append run history for %q: %w", run.IssueID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	row, err := insertRunHistory(ctx, tx, run)
	if err != nil {
		return RunHistory{}, err
	}

	if err := insertRunModelUsage(ctx, tx, row.ID, run.ModelUsage, run.ModelCosts); err != nil {
		return RunHistory{}, err
	}

	if err := insertRunReportedAmounts(ctx, tx, row.ID, run.ReportedAmounts); err != nil {
		return RunHistory{}, err
	}

	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+runEventsSavepoint); err != nil {
		return RunHistory{}, fmt.Errorf("savepoint run events for %q: %w", run.IssueID, err)
	}

	dispatchID := ""
	if run.DispatchID != nil {
		dispatchID = *run.DispatchID
	}
	eventsErr := storeEvents(ctx, tx, dispatchID, run.CompletedAt, events, limits)
	if eventsErr != nil {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO "+runEventsSavepoint); err != nil {
			return RunHistory{}, fmt.Errorf("roll back run events for %q: %w", run.IssueID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "RELEASE "+runEventsSavepoint); err != nil {
		return RunHistory{}, fmt.Errorf("release run events savepoint for %q: %w", run.IssueID, err)
	}

	if err := tx.Commit(); err != nil {
		return RunHistory{}, fmt.Errorf("commit append run history for %q: %w", run.IssueID, err)
	}
	if eventsErr != nil {
		return row, &RunEventsError{Err: eventsErr}
	}
	return row, nil
}

// insertRunReportedAmounts inserts one run_reported_amounts row per amount for
// runID.
func insertRunReportedAmounts(ctx context.Context, ex statementExecer, runID int64, amounts []ReportedAmount) error {
	for _, amount := range amounts {
		if _, err := ex.ExecContext(ctx,
			`INSERT INTO run_reported_amounts (run_id, unit, model, amount, lower_bound) VALUES (?, ?, ?, ?, ?)`,
			runID, amount.Unit, amount.Model, amount.Amount, amount.LowerBound,
		); err != nil {
			return fmt.Errorf("append run reported amount for run %d unit %q: %w", runID, amount.Unit, err)
		}
	}
	return nil
}

// AppendRunEvents stores events under dispatchID for a run whose history row
// is stored, in one transaction, then prunes under limits; completedAt is
// that row's completed_at.
func (s *Store) AppendRunEvents(ctx context.Context, dispatchID, completedAt string, events []RunEvent, limits RunEventLimits) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append run events %q: %w", dispatchID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	if err := storeEvents(ctx, tx, dispatchID, completedAt, events, limits); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append run events %q: %w", dispatchID, err)
	}
	return nil
}

// RunEvents returns one run's stored events ascending by seq: all when newest
// is 0, else the newest up to newest. It returns an empty non-nil slice when
// the run has none.
func (s *Store) RunEvents(ctx context.Context, dispatchID string, newest int) ([]RunEvent, error) {
	query := `SELECT seq, at, event, payload, size FROM run_events WHERE dispatch_id = ? ORDER BY seq`
	args := []any{dispatchID}
	if newest > 0 {
		query = `SELECT seq, at, event, payload, size FROM (
				SELECT seq, at, event, payload, size FROM run_events WHERE dispatch_id = ? ORDER BY seq DESC LIMIT ?
			) ORDER BY seq`
		args = append(args, newest)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query run events %q: %w", dispatchID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	events := []RunEvent{}
	for rows.Next() {
		var e RunEvent
		if err := rows.Scan(&e.Seq, &e.At, &e.Event, &e.Payload, &e.Size); err != nil {
			return nil, fmt.Errorf("scan run event %q: %w", dispatchID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query run events %q: %w", dispatchID, err)
	}
	return events, nil
}

// storeEvents inserts events under id, keeps run_events_total in step, and
// prunes the events of other runs under limits. It is the only writer of
// run_events and run_events_total, and runs inside the caller's transaction.
func storeEvents(ctx context.Context, tx *sql.Tx, id, completedAt string, events []RunEvent, limits RunEventLimits) error {
	if id == "" {
		return errors.New("store run events: empty dispatch ID")
	}
	completed, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		return fmt.Errorf("store run events %q: parse completed_at: %w", id, err)
	}

	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO run_events (dispatch_id, seq, at, event, completed_at, size, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert run events %q: %w", id, err)
	}
	defer insert.Close() //nolint:errcheck // statement of a transaction that ends right after

	var inserted int64
	for _, e := range events {
		if _, err := insert.ExecContext(ctx, id, e.Seq, e.At, e.Event, completedAt, e.Size, e.Payload); err != nil {
			return fmt.Errorf("insert run event %q seq %d: %w", id, e.Seq, err)
		}
		inserted += e.Size
	}

	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT bytes FROM run_events_total WHERE id = 1`).Scan(&total); err != nil {
		return fmt.Errorf("read run events total: %w", err)
	}
	total += inserted

	cutoff := completed.UTC().Add(-limits.MaxAge).Format(time.RFC3339)
	taken, takenSize, err := runsToPrune(ctx, tx, id, cutoff, total, limits)
	if err != nil {
		return err
	}
	for _, dispatchID := range taken {
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_events WHERE dispatch_id = ?`, dispatchID); err != nil {
			return fmt.Errorf("delete run events %q: %w", dispatchID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE run_events_total SET bytes = ? WHERE id = 1`, total-takenSize); err != nil {
		return fmt.Errorf("update run events total: %w", err)
	}
	return nil
}

// runsToPrune walks the index in completed_at order, skipping keep, and
// returns the leading runs to delete with their summed size. It takes a run
// while the run completed before cutoff and fewer than MaxExpiredRuns runs
// are taken, or while the total less the taken size exceeds MaxBytes, and
// stops at the first run neither condition takes. The walk reads the index
// only and is closed before it returns, so the caller may write.
func runsToPrune(ctx context.Context, tx *sql.Tx, keep, cutoff string, total int64, limits RunEventLimits) ([]string, int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT completed_at, dispatch_id, size FROM run_events WHERE dispatch_id <> ? ORDER BY completed_at, dispatch_id`, keep)
	if err != nil {
		return nil, 0, fmt.Errorf("walk run events: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var taken []string
	var takenSize int64
	current := ""
	for rows.Next() {
		var completedAt, dispatchID string
		var size int64
		if err := rows.Scan(&completedAt, &dispatchID, &size); err != nil {
			return nil, 0, fmt.Errorf("scan run events walk: %w", err)
		}
		if dispatchID != current {
			takeExpired := completedAt < cutoff && len(taken) < limits.MaxExpiredRuns
			takeExcess := total-takenSize > limits.MaxBytes
			if !takeExpired && !takeExcess {
				return taken, takenSize, nil
			}
			current = dispatchID
			taken = append(taken, dispatchID)
		}
		takenSize += size
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("walk run events: %w", err)
	}
	return taken, takenSize, nil
}
