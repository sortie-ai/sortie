//go:build unix

package qualification

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

// ProcessGroupPresent reports whether the kernel still has a process
// group led by pgid, using the Unix signal-zero liveness primitive on
// the exact negative PGID. A nil result with no error means a member
// survives; a query error other than group absence is a runtime
// failure, never a clean absence.
//
// pgid must be a positive group id. The negation this query relies on
// gives every other value a different meaning that would answer a
// question the caller did not ask: 0 negates to 0, which addresses the
// caller's own process group and so reports present for as long as the
// test process lives; 1 negates to -1, which addresses every process
// the caller may signal; a negative value negates to a positive pid and
// addresses one process rather than a group. Each would report a
// liveness that is not the launched group's, so they are rejected
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

// AwaitProcessGroupAbsence polls the exact negative PGID with the
// signal-zero primitive until the group is absent or the shared
// ShutdownDeadline elapses. A survivor or a query error fails the
// test; the caller records the outcome in the cleanup evidence without
// ever naming the PGID.
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
