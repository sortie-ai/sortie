//go:build unix

package workspace

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive lock on f without waiting and reports
// whether it got it. Callers open the lock file afresh for every
// reservation: a flock lock belongs to the open file description, so
// goroutines of one process exclude each other only when they hold separate
// descriptions.
func tryLockFile(f *os.File) (locked bool, err error) {
	fd := descriptor(f)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}

// unlockFile drops the lock on f. Closing the file drops it on every
// platform, so a failure here changes nothing.
func unlockFile(f *os.File) {
	_ = syscall.Flock(descriptor(f), syscall.LOCK_UN) //nolint:errcheck // closing the file releases the lock regardless
}

func descriptor(f *os.File) int {
	return int(f.Fd()) //nolint:gosec // G115: a Unix file descriptor is a small non-negative int carried as uintptr
}
