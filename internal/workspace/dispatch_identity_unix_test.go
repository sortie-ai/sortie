//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
	case <-time.After(5 * time.Second):
		t.Fatal("ReadDispatchSessionID blocked on a FIFO with no writer, want a non-blocking open")
	}

	assertWarnReason(t, getLog(), "not_regular")
}
