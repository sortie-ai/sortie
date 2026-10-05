//go:build unix

package procgroup

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

// Present reports whether the kernel still has a process group led by pgid;
// an error other than group absence is a runtime failure. pgid must exceed
// 1, since 0, 1, and negative values target a different liveness.
func Present(pgid int) (bool, error) {
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

// Signal sends sig to the process group led by pgid and reports nil when
// the group is already gone. It addresses the group by number alone, so
// it is for tooling that sweeps a recorded group after its launch ended.
// pgid must exceed 1, since 0, 1, and negative values target other
// processes.
func Signal(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("process group id must be greater than 1, got %d", pgid)
	}
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// AwaitAbsence polls the negative PGID until the group is absent or
// ShutdownDeadline elapses. A survivor or a query error fails the test.
func AwaitAbsence(t *testing.T, pgid int) {
	t.Helper()

	deadline := time.Now().Add(ShutdownDeadline)
	for {
		present, err := Present(pgid)
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
