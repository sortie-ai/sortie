//go:build unix

package probe

import (
	"context"
	"errors"
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
	"github.com/sortie-ai/sortie/internal/agent/agenttest/launchtest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/tools/qualify/e2e"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
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

// register records g as the launch record of a process group this run
// leads, admitting its members and whatever they start into the ledger.
func (o *ownedDescendants) register(g *procutil.Group) {
	o.tracker.register(g)
}

// readHeld is the only place the process table is read for the ledger. A
// record released before or during the reading is left out of held: its
// number may already name another process.
func (o *ownedDescendants) readHeld() (members []processMember, held []int, err error) {
	kept := procutil.HeldThrough(o.tracker.all(), func() {
		members, err = psSnapshot()
	})
	for _, g := range kept {
		held = append(held, g.PID())
	}
	return members, held, err
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

// observe folds one snapshot into the ownership set, admitting the members
// of every group in pgids, and returns every owned process still running
// in it.
func (o *ownedDescendants) observe(members []processMember, pgids []int) []processMember {
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

// descendantsOf returns the members that descend from root through
// parent links, root itself excluded.
func descendantsOf(members []processMember, root int) []processMember {
	admitted := map[int]bool{root: true}
	var found []processMember
	for changed := true; changed; {
		changed = false
		for _, m := range members {
			if !admitted[m.pid] && admitted[m.ppid] {
				admitted[m.pid] = true
				found = append(found, m)
				changed = true
			}
		}
	}
	return found
}

// signalLaunch sends sig to g's process group. SIGKILL leaves the runtime
// no shutdown path of its own, so whatever it had started in a session of
// its own, as a shell tool does, would outlive the launch and be graded
// against a teardown that never ran: the probe ends those itself.
func signalLaunch(owned *ownedDescendants, g *procutil.Group, sig syscall.Signal) {
	if sig != syscall.SIGKILL {
		_ = g.Signal(sig)
		return
	}
	var detached []processMember
	members, held, err := owned.readHeld()
	if err == nil && slices.Contains(held, g.PID()) {
		owned.observe(members, held)
		for _, m := range descendantsOf(members, g.PID()) {
			if m.pgid != g.PID() {
				detached = append(detached, m)
			}
		}
	}
	_ = g.Signal(sig)
	killMatching(detached)
}

// killMatching kills each of want that still runs with the same pid, group
// and program name, so a reused pid is never signalled. The parent is not
// compared: a child of the killed leader has been reparented by now.
func killMatching(want []processMember) {
	if len(want) == 0 {
		return
	}
	running, err := psSnapshot()
	if err != nil {
		return
	}
	for _, m := range running {
		if slices.ContainsFunc(want, func(w processMember) bool {
			return w.pid == m.pid && w.pgid == m.pgid && w.basename == m.basename
		}) {
			_ = syscall.Kill(m.pid, syscall.SIGKILL)
		}
	}
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

	snapshot, heldGroups, err := fixture.ownership().readHeld()
	if err != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
	}
	observed := 0
	for _, pgid := range heldGroups {
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
	beforeStop, heldBefore, snapshotErr := owned.readHeld()
	if snapshotErr == nil {
		owned.observe(beforeStop, heldBefore)
	}

	stopped, err := fixture.stopOpenSessions(context.Background())
	if err != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "a session did not complete its own shutdown path"}
	}
	if snapshotErr != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
	}
	checked := len(fixture.tracker.all())
	if checked == 0 {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the run registered no launch, so its teardown has no subject"}
	}

	var settled evidence.Observation
	var survivorCount int
	err = waittest.UntilWithin(cleanupSettleWindow, cleanupPollInterval, func() error {
		owned.ingestReceipts()
		snapshot, held, snapErr := owned.readHeld()
		if snapErr != nil {
			settled = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the process-group membership query failed"}
			return nil
		}
		survivors := owned.observe(snapshot, held)
		if len(survivors) > 0 {
			survivorCount = len(survivors)
			return fmt.Errorf("%d process(es) still running", survivorCount)
		}
		if failed := releasedWithCleanupError(fixture.tracker.all()); failed > 0 {
			settled = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: fmt.Sprintf("%d launch(es) were released with a cleanup error", failed)}
			return nil
		}
		settled = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: fmt.Sprintf("checked_groups=%d stopped_sessions=%d", checked, stopped)}
		return nil
	})
	if err != nil {
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: fmt.Sprintf("%d process(es) the run launched, or that a launch of it started, were still running after their own shutdown path", survivorCount)}
	}
	return settled
}

// drainOwnedProcesses kills whatever the run left running: each launch
// through its record, an adopted descendant by pid, never by name or
// command pattern, so it reaches nothing the run did not start.
func drainOwnedProcesses(owned *ownedDescendants) {
	_ = waittest.UntilWithin(e2e.ShutdownDeadline, cleanupPollInterval, func() error {
		for _, g := range owned.tracker.all() {
			_ = g.Kill()
		}
		snapshot, held, err := owned.readHeld()
		if err != nil {
			return nil
		}
		survivors := owned.observe(snapshot, held)
		for _, m := range survivors {
			_ = syscall.Kill(m.pid, syscall.SIGKILL)
		}
		if len(survivors) > 0 {
			return fmt.Errorf("%d process(es) survived the kill", len(survivors))
		}
		return nil
	})
}

// releasedWithCleanupError counts the records whose reap reported a
// cleanup error. A released record is out of every process-table read,
// so a member that outlived its reap shows up only here; a record still
// held stays with the process-table read.
func releasedWithCleanupError(groups []*procutil.Group) int {
	failed := 0
	for _, g := range groups {
		if err := launchtest.ReleaseError(g.Released()); err != nil && !errors.Is(err, launchtest.ErrNotReleased) {
			failed++
		}
	}
	return failed
}
