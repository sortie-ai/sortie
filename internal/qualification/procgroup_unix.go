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
func ProcessGroupPresent(pgid int) (bool, error) {
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
