package procutil

import (
	"log/slog"
	"os/exec"
	"path/filepath"
	"time"
)

// groupDrainBound bounds each wait for a terminated process group to
// hold no live process, or a terminated Job Object no running one, and
// the wait of a POSIX [Group.Kill] for the launch's reap to take over.
// Only a test replaces it, to shorten the wait for a group that never
// settles.
var groupDrainBound = 2 * time.Second

// groupDrainPollInterval is the pause between successive membership
// polls while waiting for a terminated process group or Job Object to
// settle.
const groupDrainPollInterval = 5 * time.Millisecond

// Reaper reaps one started subprocess and terminates its process group.
type Reaper struct {
	done       chan struct{}
	err        error
	leftover   bool
	cleanupErr error
}

// StartReaper starts one goroutine that reaps cmd, terminates its
// process group or Job Object, unregisters the launch, and only then
// closes the channel Done returns. On POSIX the termination runs after
// the direct child has exited and before it is reaped; on Windows it
// runs after the reap.
//
// cmd MUST have been started by a procutil start function such as
// [StartWithOwnedPipes], and StartReaper MUST be called once per
// launch: it panics for a command no such function started and on a
// second call for the same launch.
//
// A termination that could not prove the process tree torn down is
// logged as the one [CaptureCleanupWarning] record for this reap; every
// caller sees that record regardless of which one of them later reads
// [Reaper.CleanupErr]. If logger is nil, StartReaper uses
// [slog.Default].
func StartReaper(cmd *exec.Cmd, logger *slog.Logger) *Reaper {
	g := lookupGroup(cmd)
	if g == nil {
		panic("procutil: StartReaper requires a command started by a procutil start function")
	}
	if g.claimReaper() {
		panic("procutil: StartReaper called twice for one launch")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reaper{done: make(chan struct{})}
	go func() {
		r.err, r.leftover, r.cleanupErr = g.reap()
		groups.Delete(cmd)

		// A termination that failed leaves the process tree unproven, so
		// this record is the only thing standing between a surviving
		// descendant and a launch that looks cleanly torn down.
		if r.cleanupErr != nil {
			logger.Warn(CaptureCleanupWarning,
				slog.String("command", filepath.Base(cmd.Path)),
				slog.Any("error", r.cleanupErr))
		}

		close(r.done)
	}()
	return r
}

// Done closes once the subprocess has been reaped, its process group
// or Job Object terminated, and its launch record released.
func (r *Reaper) Done() <-chan struct{} {
	return r.done
}

// Err returns the error cmd.Wait reported. Call only after Done closed.
func (r *Reaper) Err() error {
	return r.err
}

// Leftover reports whether the reap found a live process of cmd's
// process group or Job Object other than the direct child. On POSIX
// that is a live member observed before or during the reap's group
// drain; on Windows it is a running Job Object member. Call only after
// Done closed.
func (r *Reaper) Leftover() bool {
	return r.leftover
}

// CleanupErr returns the error the termination reported, or nil when
// it succeeded or found the group already gone. It covers a bounded
// wait for the group to hold no live process (the Job Object no running
// one) timing out and, on POSIX, a failure to observe the direct
// child's exit, in which case nothing was sent to the group. Call only
// after Done closed.
//
// A non-nil value means the reap could not prove the process tree was
// torn down, so a descendant may have survived it, whether or not
// [Reaper.Leftover] is set.
func (r *Reaper) CleanupErr() error {
	return r.cleanupErr
}
