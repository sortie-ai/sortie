//go:build unix

package probe

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// processMember is one row of a ps snapshot.
type processMember struct {
	pid, pgid, ppid int
	basename        string
}

// psSnapshotBound bounds one process-table query, so a query that never
// returns cannot hold a bounded wait open past its deadline.
const psSnapshotBound = 10 * time.Second

// psSnapshot runs one bounded ps query and parses every row, so a
// reading over process-group membership comes from a single call. A
// process the kernel has ended and not yet reaped is left out: it holds
// a table row, not a running program.
func psSnapshot() ([]processMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psSnapshotBound)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,ppid=,stat=,args=").Output() //nolint:gosec // fixed command and fixed arguments
	if err != nil {
		return nil, err
	}
	var members []processMember
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pgid, err2 := strconv.Atoi(fields[1])
		ppid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if strings.HasPrefix(fields[3], "Z") {
			continue
		}
		name := filepath.Base(fields[4])
		// A shebang script's own basename is the argument token after
		// the interpreter, not the interpreter itself.
		if (name == "sh" || name == "bash") && len(fields) > 5 {
			name = filepath.Base(fields[5])
		}
		members = append(members, processMember{pid: pid, pgid: pgid, ppid: ppid, basename: name})
	}
	return members, nil
}

// ownedDescendants is the set of processes a run can attribute to
// itself. A launched group proves ownership by its registered group id;
// a descendant that starts a group of its own proves it through its
// parent. Ownership carries forward across snapshots and is dropped
// only when the process leaves the table, so a reused pid is never
// inherited.
//
// The tracker is read on every snapshot rather than copied once, so a
// launch registered after the ledger exists admits from its own group.
type ownedDescendants struct {
	tracker *groupTracker

	mu       sync.Mutex
	owned    map[int]bool // guarded by mu
	receipts []string     // guarded by mu
}

func newOwnedDescendants(tracker *groupTracker) *ownedDescendants {
	return &ownedDescendants{tracker: tracker, owned: map[int]bool{}}
}

// register records pgid as a process group a launch of this run leads,
// admitting its members and whatever they start into the ledger.
func (o *ownedDescendants) register(pgid int) {
	o.tracker.register(pgid)
}

func (o *ownedDescendants) groups() []int {
	return o.tracker.all()
}

// watchReceipts registers the descendant receipts of the programs this
// run authored. Each names its own live children as it exits, the last
// moment they are provably its, so a child a periodic process-table
// reading missed is still charged to the run. Registering a path
// discards whatever sits there first, since nothing this run started
// can have written it yet.
func (o *ownedDescendants) watchReceipts(commandPaths ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, path := range commandPaths {
		if path == "" {
			continue
		}
		receipt := agenttest.DescendantReceipt(path)
		if slices.Contains(o.receipts, receipt) {
			continue
		}
		_ = os.Remove(receipt)
		o.receipts = append(o.receipts, receipt)
	}
}

// ingestReceipts charges this run with every process id its own
// programs recorded as still running when they exited.
func (o *ownedDescendants) ingestReceipts() {
	o.mu.Lock()
	receipts := slices.Clone(o.receipts)
	o.mu.Unlock()

	var recorded []int
	for _, receipt := range receipts {
		recorded = append(recorded, recordedDescendants(receipt)...)
	}
	if len(recorded) == 0 {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	for _, pid := range recorded {
		o.owned[pid] = true
	}
}

// recordedDescendants reads the process ids one program appended to its
// receipt. An absent receipt means that program never ran, left nothing
// running, or was killed before it could record anything.
func recordedDescendants(receipt string) []int {
	raw, err := os.ReadFile(receipt) //nolint:gosec // the receipt of a program this run itself wrote and launched
	if err != nil {
		return nil
	}
	var pids []int
	for line := range strings.SplitSeq(string(raw), "\n") {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// observe folds one snapshot into the ownership set and returns every
// owned process still running in it.
func (o *ownedDescendants) observe(members []processMember) []processMember {
	pgids := o.groups()

	o.mu.Lock()
	defer o.mu.Unlock()
	running := make(map[int]bool, len(members))
	for _, m := range members {
		running[m.pid] = true
		if slices.Contains(pgids, m.pgid) {
			o.owned[m.pid] = true
		}
	}
	maps.DeleteFunc(o.owned, func(pid int, _ bool) bool { return !running[pid] })
	for changed := true; changed; {
		changed = false
		for _, m := range members {
			if !o.owned[m.pid] && o.owned[m.ppid] {
				o.owned[m.pid] = true
				changed = true
			}
		}
	}
	var survivors []processMember
	for _, m := range members {
		if o.owned[m.pid] {
			survivors = append(survivors, m)
		}
	}
	return survivors
}
