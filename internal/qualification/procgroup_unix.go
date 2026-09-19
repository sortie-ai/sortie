//go:build unix

package qualification

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

// ProcessGroupPresent reports whether the kernel still has a process group led
// by pgid, via the signal-zero liveness query on the negative PGID. A query
// error other than group absence is a runtime failure, not a clean absence.
//
// pgid must be greater than 1: 0, 1, and negative values negate to targets
// (the caller's own group, every signalable process, a single pid) that would
// report a liveness other than the launched group's, so they are rejected
// rather than queried.
func ProcessGroupPresent(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, fmt.Errorf("process group id must be greater than 1, got %d", pgid)
	}
	err := syscall.Kill(-pgid, syscall.Signal(0))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("signal-zero query for the launched group: %w", err)
	}
}

// AwaitProcessGroupAbsence polls the negative PGID until the group is absent or
// ShutdownDeadline elapses. A survivor or a query error fails the test.
func AwaitProcessGroupAbsence(t *testing.T, pgid int) {
	t.Helper()

	deadline := time.Now().Add(ShutdownDeadline)
	for {
		present, err := ProcessGroupPresent(pgid)
		if err != nil {
			t.Fatalf("process-group liveness query failed: %v", err)
		}
		if !present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a member of the launched process group survived the cleanup deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
