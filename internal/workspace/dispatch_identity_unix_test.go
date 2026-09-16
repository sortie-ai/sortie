//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// timeoutChan returns a channel that fires after a generous bound, used
// to fail a test that would otherwise hang forever on an unexpected
// blocking read.
func timeoutChan(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}

// TestReadDispatchSessionID_FIFOAtRecordPath proves a FIFO at the
// record path is rejected as not_regular rather than opened and waited
// on: ReadDispatchSessionID must return promptly without blocking for a
// writer that never arrives.
func TestReadDispatchSessionID_FIFOAtRecordPath(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}
	fifoPath := filepath.Join(ws, sortieDir, dispatchIdentityFile)
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo unavailable in this test environment: %v", err)
	}

	logger, getLog := slogCapture()

	done := make(chan string, 1)
	go func() { done <- ReadDispatchSessionID(ws, "D1", logger) }()

	select {
	case got := <-done:
		if got != "" {
			t.Errorf("ReadDispatchSessionID(FIFO at record) = %q, want empty", got)
		}
	case <-timeoutChan(t):
		t.Fatal("ReadDispatchSessionID blocked on a FIFO with no writer, want a non-blocking open")
	}

	assertWarnReason(t, getLog(), "not_regular")
}
