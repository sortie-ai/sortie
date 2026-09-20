//go:build unix

package probe

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
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

// membersDescendFromLaunch reports whether every member descends from
// the process the run launched. Each launch starts in a group of its
// own, so the registered group id is that process's pid; admission
// propagates through the parent-child levels to a fixed point.
//
// Nothing is admitted by the name it runs under: a runtime exec'd
// through an interpreter shebang reports the interpreter, so no name a
// coordinate could supply ever appears.
func membersDescendFromLaunch(members []processMember, pgid int) bool {
	admitted := map[int]bool{}
	for _, m := range members {
		if m.pid == pgid {
			admitted[m.pid] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, m := range members {
			if !admitted[m.pid] && admitted[m.ppid] {
				admitted[m.pid] = true
				changed = true
			}
		}
	}
	for _, m := range members {
		if !admitted[m.pid] {
			return false
		}
	}
	return true
}

func membersOfGroup(members []processMember, pgid int) []processMember {
	var group []processMember
	for _, m := range members {
		if m.pgid == pgid {
			group = append(group, m)
		}
	}
	return group
}

// configRoots returns every directory a project config could be read
// from: each launch directory, its parents up to root, and root.
// Launches run in subdirectories of root, so reading root alone would
// miss a config where the runtime started.
func configRoots(root string, launches []string) []string {
	roots := []string{root}
	for _, launch := range launches {
		for dir := launch; dir != root && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if !slices.Contains(roots, dir) {
				roots = append(roots, dir)
			}
		}
	}
	return roots
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

// induceWorkspaceSecurity reads the three facts this run can observe
// about where its launches ran: every launch directory resolved inside
// the run-scoped root, no project_config_paths entry applied to any of
// them, and every observed process-group member descended from the
// launched command. This is not an OS sandbox and nothing here observes
// what a launch could reach outside its directory, so it claims
// neither.
func induceWorkspaceSecurity(t *testing.T, coords Coordinates, fixture *sharedFixture) qualification.Observation {
	t.Helper()

	launches := fixture.launchDirs()
	for _, launch := range launches {
		if !pathWithin(launch, fixture.workspaceRoot) {
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "a launch ran in a directory outside the run-scoped root"}
		}
	}
	for _, dir := range configRoots(fixture.workspaceRoot, launches) {
		for _, rel := range coords.Profile.ProjectConfigPaths {
			if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
				return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "a project_config_paths entry applies to a directory a launch ran in"}
			}
		}
	}

	snapshot, err := psSnapshot()
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
	}
	observed := 0
	for _, pgid := range fixture.tracker.all() {
		group := membersOfGroup(snapshot, pgid)
		if !membersDescendFromLaunch(group, pgid) {
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "an unexpected helper was observed in a launched process group"}
		}
		observed += len(group)
	}
	if observed == 0 {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "no member of any launched process group was still running to be read"}
	}

	return qualification.Observation{
		Grade:   qualification.GradeUsable,
		Outcome: qualification.OutcomePass,
		Detail:  fmt.Sprintf("every launch ran in a directory inside the run-scoped root; no project settings applied to any of them; all %d observed process-group members were the launched command or a descendant of it", observed),
	}
}

// cleanupSettleWindow is how long a launched process may still hold a
// process-table row after the shutdown path returned. Each path waits
// for the exit itself, so this absorbs only the gap before the kernel
// drops the row, never a slow teardown.
const cleanupSettleWindow = 2 * time.Second

const cleanupPollInterval = 100 * time.Millisecond

// induceProcessCleanup grades this run's teardown: it reads which
// processes the run owns while its launches still run, stops every
// session, then reads whether any owned process is still running. The
// drain that follows kills what the reading found and never revises it.
//
// A pass is bounded by the set this run can attribute to itself: a
// descendant that detaches into its own group and loses the link before
// a reading catches it is outside that set. So a pass states that
// nothing the run could attribute to itself outlived its shutdown path,
// not that the runtime left no process behind.
func induceProcessCleanup(fixture *sharedFixture) qualification.Observation {
	owned := fixture.ownership()
	defer drainOwnedProcesses(owned)
	return observeProcessCleanup(fixture, owned)
}

func observeProcessCleanup(fixture *sharedFixture, owned *ownedDescendants) qualification.Observation {
	// Read while the sessions still run, so their descendants are seen
	// while the parents they are traced through are still there for the
	// shutdown path below to end.
	beforeStop, snapshotErr := psSnapshot()
	if snapshotErr == nil {
		owned.observe(beforeStop)
	}

	stopped, err := fixture.stopOpenSessions(context.Background())
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "a session did not complete its own shutdown path"}
	}
	if snapshotErr != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
	}
	groups := owned.groups()
	if len(groups) == 0 {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the run registered no launch, so its teardown has no subject"}
	}

	deadline := time.Now().Add(cleanupSettleWindow)
	for {
		owned.ingestReceipts()
		snapshot, snapErr := psSnapshot()
		if snapErr != nil {
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
		}
		survivors := owned.observe(snapshot)
		if len(survivors) == 0 {
			return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: fmt.Sprintf("checked_groups=%d stopped_sessions=%d", len(groups), stopped)}
		}
		if time.Now().After(deadline) {
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: fmt.Sprintf("%d process(es) the run launched, or that a launch of it started, were still running after their own shutdown path", len(survivors))}
		}
		time.Sleep(cleanupPollInterval)
	}
}

// drainOwnedProcesses kills whatever the run left running: every
// launched process group and every process ownership was proved for. It
// is addressed by pid, never by name or command pattern, so it can
// reach nothing the run did not start.
func drainOwnedProcesses(owned *ownedDescendants) {
	deadline := time.Now().Add(qualification.ShutdownDeadline)
	for {
		for _, pgid := range owned.groups() {
			_ = signalProcessGroup(pgid, syscall.SIGKILL)
		}
		snapshot, err := psSnapshot()
		if err != nil {
			return
		}
		survivors := owned.observe(snapshot)
		if len(survivors) == 0 {
			return
		}
		for _, m := range survivors {
			_ = syscall.Kill(m.pid, syscall.SIGKILL)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(cleanupPollInterval)
	}
}
