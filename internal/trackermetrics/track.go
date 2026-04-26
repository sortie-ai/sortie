// Package trackermetrics provides shared tracker-operation metric helpers.
package trackermetrics

import "github.com/sortie-ai/sortie/internal/domain"

// Track records the success or failure of a logical tracker operation and returns fn's error unchanged.
func Track(rec domain.Metrics, op string, fn func() error) error {
	err := fn()
	if rec == nil {
		return err
	}

	if err != nil {
		rec.IncTrackerRequests(op, "error")
		return err
	}

	rec.IncTrackerRequests(op, "success")
	return nil
}
