package clientprotocol

import (
	"sync"
	"time"
)

// originRetention is the age at which record evicts a creation
// instant. Only record evicts, so a process that stops creating
// sessions keeps whatever its last calls left behind: that residue is
// bounded by one window's creates, and costs less than owning a
// ticker. The value exceeds loadDeferral's maximum output so an entry
// is never evicted while it could still require a deferral.
const originRetention = 2 * time.Minute

// originKey identifies one session this process created. The session
// identifier alone does not identify it: it is opaque and vendor
// assigned, and a runtime that numbers sessions per workspace can hand
// out the same one in two different workspaces.
type originKey struct {
	cwd       string
	sessionID string
}

// sessionOrigins is the creation ledger: it records when this process
// created each session. Its zero value is ready to use.
type sessionOrigins struct {
	mu      sync.Mutex
	created map[originKey]time.Time
	now     func() time.Time
}

// record notes that this process created sessionID against cwd at the
// current time, and evicts every entry older than originRetention. It
// does nothing when sessionID is empty, since an empty identifier is
// never loaded.
func (o *sessionOrigins) record(cwd, sessionID string) {
	if sessionID == "" {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	at := o.clock()
	if o.created == nil {
		o.created = make(map[originKey]time.Time)
	}
	for key, recorded := range o.created {
		if at.Sub(recorded) > originRetention {
			delete(o.created, key)
		}
	}
	o.created[originKey{cwd: cwd, sessionID: sessionID}] = at
}

// createdAt reports the instant this process created sessionID against
// cwd, and whether such an entry is present. It performs no eviction of
// its own.
func (o *sessionOrigins) createdAt(cwd, sessionID string) (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	at, ok := o.created[originKey{cwd: cwd, sessionID: sessionID}]
	return at, ok
}

// clock reports the current time. It is the only clock the guard reads,
// so tests can substitute a fake by setting now.
func (o *sessionOrigins) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// loadDeferral reports how long a session/load for a session created at
// createdAt must wait before now leaves createdAt's UTC minute. It
// returns zero once now reaches or passes that minute's boundary, and
// clamps to one minute when now precedes createdAt, which only happens
// after a backward wall-clock step.
func loadDeferral(createdAt, now time.Time) time.Duration {
	boundary := createdAt.UTC().Truncate(time.Minute).Add(time.Minute)
	wait := boundary.Sub(now.UTC())
	if wait <= 0 {
		return 0
	}
	if wait > time.Minute {
		return time.Minute
	}
	return wait
}
