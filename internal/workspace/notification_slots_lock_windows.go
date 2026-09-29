//go:build windows

package workspace

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes an exclusive lock on byte 0 of f without waiting and
// reports whether it got it. Callers open the lock file afresh for every
// reservation, so goroutines of one process exclude each other through
// separate handles as processes do.
func tryLockFile(f *os.File) (locked bool, err error) {
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	default:
		return false, err
	}
}

// unlockFile drops the lock on f. Closing the file drops it too, so a
// failure here changes nothing.
func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{}) //nolint:errcheck // closing the file releases the lock regardless
}
