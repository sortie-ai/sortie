//go:build unix && !linux && !darwin

package procutil

import (
	"fmt"
	"runtime"
)

func errUnsupportedGOOS() error {
	return fmt.Errorf("process group membership cannot be observed on %s", runtime.GOOS)
}

// observeLeaderExit always fails, so the reap of every launch on this
// platform releases its record at once and reaps the direct child with
// no group termination.
func observeLeaderExit(_ int) error {
	return errUnsupportedGOOS()
}

// hasLiveMember is never reached: the reap takes the failed exit
// observation arm first.
func hasLiveMember(_, _ int) (bool, error) {
	return false, errUnsupportedGOOS()
}
