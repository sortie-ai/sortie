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
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
)

// processMember is one row of a ps snapshot.
type processMember struct {
	pid, pgid, ppid int
	basename        string
}

// psSnapshotBound bounds one process-table query, so a query that never
// returns cannot hold a bounded wait open past its deadline.
const psSnapshotBound = 10 * time.Second

// psSnapshot runs one bounded ps query and parses every row. A zombie
// process is left out: it holds a table row, not a running program.
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

// membersDescendFromLaunch admits nothing by name: a runtime exec'd
// through an interpreter shebang reports the interpreter, never a
// coordinate name.
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
// from: each launch directory up to root, and root itself, since
// launches run in subdirectories root alone would miss.
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

// ownedDescendants drops ownership once a process leaves the table, so
// a reused pid is never inherited. The tracker is read live, so a
// launch registered after the ledger exists still admits.
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

// Each receipt names its program's own live children on exit, the
// last provable moment, catching a child a periodic reading missed.
// Registering a path discards whatever sits there, since this run
// hasn't written it yet.
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

// induceWorkspaceSecurity is not an OS sandbox: nothing here observes
// what a launch could reach outside its directory.
func induceWorkspaceSecurity(t *testing.T, coords Coordinates, fixture *sharedFixture) evidence.Observation {
	t.Helper()

	launches := fixture.launchDirs()
	for _, launch := range launches {
		if !pathWithin(launch, fixture.workspaceRoot) {
			return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "a launch ran in a directory outside the run-scoped root"}
		}
	}
	for _, dir := range configRoots(fixture.workspaceRoot, launches) {
		for _, rel := range coords.Profile.ProjectConfigPaths {
			if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
				return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "a project_config_paths entry applies to a directory a launch ran in"}
			}
		}
	}

	snapshot, err := psSnapshot()
	if err != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
	}
	observed := 0
	for _, pgid := range fixture.tracker.all() {
		group := membersOfGroup(snapshot, pgid)
		if !membersDescendFromLaunch(group, pgid) {
			return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "an unexpected helper was observed in a launched process group"}
		}
		observed += len(group)
	}
	if observed == 0 {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "no member of any launched process group was still running to be read"}
	}

	return evidence.Observation{
		Grade:   evidence.GradeUsable,
		Outcome: evidence.OutcomePass,
		Detail:  fmt.Sprintf("every launch ran in a directory inside the run-scoped root; no project settings applied to any of them; all %d observed process-group members were the launched command or a descendant of it", observed),
	}
}

// cleanupSettleWindow absorbs only the gap before the kernel drops a
// process-table row after exit, never a slow teardown: each shutdown
// path already waits for the exit itself.
const cleanupSettleWindow = 2 * time.Second

const cleanupPollInterval = 100 * time.Millisecond

// induceProcessCleanup's pass is bounded by the run's ownership set,
// not a claim that the runtime left nothing behind system-wide.
func induceProcessCleanup(fixture *sharedFixture) evidence.Observation {
	owned := fixture.ownership()
	defer drainOwnedProcesses(owned)
	return observeProcessCleanup(fixture, owned)
}

func observeProcessCleanup(fixture *sharedFixture, owned *ownedDescendants) evidence.Observation {
	// Read while the sessions still run, so their descendants are seen
	// while the parents they are traced through are still there for the
	// shutdown path below to end.
	beforeStop, snapshotErr := psSnapshot()
	if snapshotErr == nil {
		owned.observe(beforeStop)
	}

	stopped, err := fixture.stopOpenSessions(context.Background())
	if err != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "a session did not complete its own shutdown path"}
	}
	if snapshotErr != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
	}
	groups := owned.groups()
	if len(groups) == 0 {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the run registered no launch, so its teardown has no subject"}
	}

	deadline := time.Now().Add(cleanupSettleWindow)
	for {
		owned.ingestReceipts()
		snapshot, snapErr := psSnapshot()
		if snapErr != nil {
			return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
		}
		survivors := owned.observe(snapshot)
		if len(survivors) == 0 {
			return evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: fmt.Sprintf("checked_groups=%d stopped_sessions=%d", len(groups), stopped)}
		}
		if time.Now().After(deadline) {
			return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: fmt.Sprintf("%d process(es) the run launched, or that a launch of it started, were still running after their own shutdown path", len(survivors))}
		}
		time.Sleep(cleanupPollInterval)
	}
}

// drainOwnedProcesses kills whatever the run left running, addressed
// by pid, never by name or command pattern, so it reaches nothing the
// run did not start.
func drainOwnedProcesses(owned *ownedDescendants) {
	deadline := time.Now().Add(procgroup.ShutdownDeadline)
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
