package sshutil

import (
	"crypto/rand"
	"fmt"
	"time"
)

// supervisorPollInterval is how often the remote supervisor checks that the
// SSH connection is still up. It mirrors the sleep in the embedded script.
const supervisorPollInterval = time.Second

// exitBarrierPrefix is the remote path an exit barrier lives at, less its
// name. The directory is fixed because a carried TMPDIR would otherwise move
// the barrier away from the path the waiter reads.
const exitBarrierPrefix = "/tmp/sortie-exit-"

// NewExitBarrier returns a fresh name for [SSHOptions.ExitBarrier].
func NewExitBarrier() string {
	return rand.Text()
}

func isExitBarrierName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func exitBarrierPath(name string) string {
	return exitBarrierPrefix + name
}

// AwaitExitCommand returns the remote shell fragment that returns once the
// agent tree of the launch holding barrier has exited. It returns at once
// when the launch holds no barrier, which is the case when the launch has not
// reached its agent, has ended by itself, or runs on a host without flock.
//
// The fragment holds no state of its own: a barrier nothing holds is free, so
// the wait never outlives the tree.
func AwaitExitCommand(barrier string) string {
	if !isExitBarrierName(barrier) {
		panic(fmt.Sprintf("sshutil: AwaitExitCommand: invalid exit barrier name %q", barrier))
	}
	path := shellQuote(exitBarrierPath(barrier))
	return "if command -v flock >/dev/null 2>&1 && [ -e " + path + " ]; then flock -x " + path + " true; rm -f " + path + "; fi"
}

// TeardownBound returns the longest the remote supervisor takes, from the
// moment the connection is gone, to end an agent that ignores SIGTERM: one
// poll interval plus grace rounded up to whole seconds.
func TeardownBound(grace time.Duration) time.Duration {
	return supervisorPollInterval + time.Duration(max(wholeSeconds(grace), 0))*time.Second
}

// wholeSeconds rounds grace up to whole seconds.
func wholeSeconds(grace time.Duration) int {
	return int((grace + time.Second - 1) / time.Second)
}
