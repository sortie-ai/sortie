//go:build unix

package probe

import (
	"context"
	"fmt"
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
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

func TestMembersDescendFromLaunch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		members []processMember
		pgid    int
		want    bool
	}{
		{
			name:    "the launched process is admitted by its own identity, under whatever name it runs",
			members: []processMember{{pid: 1, pgid: 1, ppid: 0, basename: "node"}},
			pgid:    1,
			want:    true,
		},
		{
			name: "a child of the launch is admitted",
			members: []processMember{
				{pid: 1, pgid: 1, ppid: 0, basename: "node"},
				{pid: 2, pgid: 1, ppid: 1, basename: "node"},
			},
			pgid: 1,
			want: true,
		},
		{
			name: "admission propagates through multiple parent levels",
			members: []processMember{
				{pid: 1, pgid: 1, ppid: 0, basename: "node"},
				{pid: 2, pgid: 1, ppid: 1, basename: "helper"},
				{pid: 3, pgid: 1, ppid: 2, basename: "grandchild"},
			},
			pgid: 1,
			want: true,
		},
		{
			name: "a member whose parent is outside the group is rejected",
			members: []processMember{
				{pid: 1, pgid: 1, ppid: 0, basename: "node"},
				{pid: 2, pgid: 1, ppid: 99, basename: "unexpected-helper"},
			},
			pgid: 1,
			want: false,
		},
		{
			name:    "a group the run never launched admits nothing",
			members: []processMember{{pid: 2, pgid: 1, ppid: 99, basename: "node"}},
			pgid:    1,
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := membersDescendFromLaunch(tt.members, tt.pgid); got != tt.want {
				t.Errorf("membersDescendFromLaunch(%+v, %d) = %v, want %v", tt.members, tt.pgid, got, tt.want)
			}
		})
	}
}

func TestMembersOfGroup(t *testing.T) {
	t.Parallel()

	members := []processMember{
		{pid: 1, pgid: 10, basename: "a"},
		{pid: 2, pgid: 20, basename: "b"},
		{pid: 3, pgid: 10, basename: "c"},
	}
	got := membersOfGroup(members, 10)
	if len(got) != 2 || got[0].basename != "a" || got[1].basename != "c" {
		t.Errorf("membersOfGroup(members, 10) = %+v, want the two pgid=10 members in order", got)
	}
}

func TestInduceWorkspaceSecurity(t *testing.T) {
	t.Parallel()

	t.Run("a leaked project_config_paths entry grades not_observed with runtime_failed", func(t *testing.T) {
		t.Parallel()

		workspaceRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspaceRoot, ".mcp.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write leaked project config fixture: %v", err)
		}
		coords := Coordinates{Profile: qualification.RuntimeProfile{ProjectConfigPaths: []string{".mcp.json"}}}
		fixture := &sharedFixture{workspaceRoot: workspaceRoot, tracker: &groupTracker{}}

		obs := induceWorkspaceSecurity(t, coords, fixture)

		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeRuntimeFailed {
			t.Errorf("induceWorkspaceSecurity(...) = %+v, want grade %q and outcome %q", obs, qualification.GradeNotObserved, qualification.OutcomeRuntimeFailed)
		}
	})

	t.Run("no observable process-group member grades not_observed with fixture_induction_failed", func(t *testing.T) {
		t.Parallel()

		coords := Coordinates{Profile: qualification.RuntimeProfile{ProjectConfigPaths: []string{".mcp.json"}}}
		fixture := &sharedFixture{workspaceRoot: t.TempDir(), tracker: &groupTracker{}}

		obs := induceWorkspaceSecurity(t, coords, fixture)

		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceWorkspaceSecurity(...) = %+v, want grade %q and outcome %q: process provenance cannot be claimed over an empty subject", obs, qualification.GradeNotObserved, qualification.OutcomeFixtureInductionFailed)
		}
	})

	t.Run("a live group of the launched command grades usable with pass", func(t *testing.T) {
		t.Parallel()

		pgid, commandPath := startTrackedLiveGroup(t)
		tracker := &groupTracker{}
		tracker.register(pgid)
		coords := Coordinates{CommandPath: commandPath, Profile: qualification.RuntimeProfile{ProjectConfigPaths: []string{".mcp.json"}}}
		fixture := &sharedFixture{workspaceRoot: t.TempDir(), tracker: tracker}

		obs := induceWorkspaceSecurity(t, coords, fixture)

		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceWorkspaceSecurity(...) = %+v, want grade %q and outcome %q", obs, qualification.GradeUsable, qualification.OutcomePass)
		}
		if strings.Contains(obs.Detail, "isolat") || strings.Contains(obs.Detail, "sandbox") {
			t.Errorf("induceWorkspaceSecurity(...).Detail = %q, want no filesystem-isolation claim: nothing here observes what a launch could reach outside its own directory", obs.Detail)
		}
	})
}

func startTrackedLiveGroup(t *testing.T) (pgid int, commandPath string) {
	t.Helper()

	hangPath := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{Hang: true})
	cmd := exec.Command(hangPath) //nolint:gosec // hangPath is a fake runtime this test built under its own temp directory
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tracked process group: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	return cmd.Process.Pid, hangPath
}

// stubAdapter's StopSession does only what stop does, isolating what the
// shutdown path achieved from what the drain after it did.
type stubAdapter struct {
	mu    sync.Mutex
	stops int
	stop  func() error
}

func (a *stubAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{}, nil
}

func (a *stubAdapter) RunTurn(context.Context, domain.Session, domain.RunTurnParams) (domain.TurnResult, error) {
	return domain.TurnResult{}, nil
}

func (a *stubAdapter) StopSession(context.Context, domain.Session) error {
	a.mu.Lock()
	a.stops++
	stop := a.stop
	a.mu.Unlock()
	if stop == nil {
		return nil
	}
	return stop()
}

func (a *stubAdapter) stopCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stops
}

func TestInduceProcessCleanupReportsALiveGroupAsALeak(t *testing.T) {
	t.Parallel()

	pgid, _ := startTrackedLiveGroup(t)
	tracker := &groupTracker{}
	tracker.register(pgid)

	obs := induceProcessCleanup(&sharedFixture{tracker: tracker})

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation for a group that was alive when the run was graded", obs)
	}
	if present, err := qualification.ProcessGroupPresent(pgid); err != nil || present {
		t.Errorf("ProcessGroupPresent(%d) = %v, %v, want false, nil: a failing observation must still leave no child behind", pgid, present, err)
	}
}

func TestInduceProcessCleanupWithoutASubjectIsNotAPass(t *testing.T) {
	t.Parallel()

	obs := induceProcessCleanup(&sharedFixture{tracker: &groupTracker{}})

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation when no launch was registered to observe", obs)
	}
}

func TestInduceProcessCleanupStopsRegisteredSessionsBeforeReading(t *testing.T) {
	t.Parallel()

	t.Run("a session whose group survives its own stop is a leak the drain still clears", func(t *testing.T) {
		t.Parallel()

		pgid, _ := startTrackedLiveGroup(t)
		adapter := &stubAdapter{}
		fixture := &sharedFixture{tracker: &groupTracker{}}
		if err := fixture.registerSession(adapter, domain.Session{ID: "induction", AgentPID: strconv.Itoa(pgid)}); err != nil {
			t.Fatalf("registerSession(...) error = %v, want nil", err)
		}

		obs := induceProcessCleanup(fixture)

		if adapter.stopCount() != 1 {
			t.Errorf("StopSession was called %d times, want exactly one call through the session's own shutdown path", adapter.stopCount())
		}
		if obs.Grade == qualification.GradeUsable {
			t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation for a session the shutdown path left running", obs)
		}
		if present, err := qualification.ProcessGroupPresent(pgid); err != nil || present {
			t.Errorf("ProcessGroupPresent(%d) = %v, %v, want false, nil after the drain", pgid, present, err)
		}
	})

	t.Run("a session its own stop ended grades usable with pass", func(t *testing.T) {
		t.Parallel()

		pgid, _ := startTrackedLiveGroup(t)
		adapter := &stubAdapter{stop: func() error { return signalProcessGroup(pgid, syscall.SIGKILL) }}
		fixture := &sharedFixture{tracker: &groupTracker{}}
		if err := fixture.registerSession(adapter, domain.Session{ID: "induction", AgentPID: strconv.Itoa(pgid)}); err != nil {
			t.Fatalf("registerSession(...) error = %v, want nil", err)
		}

		obs := induceProcessCleanup(fixture)

		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceProcessCleanup(...) = %+v, want grade %q and outcome %q", obs, qualification.GradeUsable, qualification.OutcomePass)
		}
		if obs.Detail != "checked_groups=1 stopped_sessions=1" {
			t.Errorf("induceProcessCleanup(...).Detail = %q, want the launched group and the stopped session both counted", obs.Detail)
		}
	})
}

func TestInduceWorkspaceSecurityNoticesAConfigInTheLaunchDirectory(t *testing.T) {
	t.Parallel()

	fixture := semanticFixture(t)
	launch := fixture.newLaunchWorkspace(t)
	if err := os.WriteFile(filepath.Join(launch, ".runtime-config"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write the launch-directory config fixture: %v", err)
	}
	coords := Coordinates{Profile: qualification.RuntimeProfile{ProjectConfigPaths: []string{".runtime-config"}}}

	obs := induceWorkspaceSecurity(t, coords, fixture)

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceWorkspaceSecurity(...) = %+v, want a negative observation for a project config inside the directory the launch actually ran in", obs)
	}
}

func TestConfigRoots(t *testing.T) {
	t.Parallel()

	root := filepath.Join("/tmp", "run")
	launch := filepath.Join(root, "nested", "launch-1")
	got := configRoots(root, []string{launch, launch})
	want := []string{root, launch, filepath.Join(root, "nested")}

	if !slices.Equal(got, want) {
		t.Errorf("configRoots(%q, ...) = %v, want %v", root, got, want)
	}
}

func ownedOverGroups(pgids ...int) *ownedDescendants {
	tracker := &groupTracker{}
	for _, pgid := range pgids {
		tracker.register(pgid)
	}
	return newOwnedDescendants(tracker)
}

func TestOwnedDescendantsObserve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		members []processMember
		pgids   []int
		want    []int
	}{
		{
			name:    "a member of a launched group survives",
			members: []processMember{{pid: 2, pgid: 2, ppid: 1}},
			pgids:   []int{2},
			want:    []int{2},
		},
		{
			name: "a descendant that left the group is still charged to the launch",
			members: []processMember{
				{pid: 2, pgid: 2, ppid: 1},
				{pid: 3, pgid: 3, ppid: 2},
			},
			pgids: []int{2},
			want:  []int{2, 3},
		},
		{
			name:    "a process outside every launched group is not a survivor",
			members: []processMember{{pid: 9, pgid: 9, ppid: 1}},
			pgids:   []int{2},
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []int
			for _, m := range ownedOverGroups(tt.pgids...).observe(tt.members) {
				got = append(got, m.pid)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("observe(%+v) over groups %v = %v, want %v", tt.members, tt.pgids, got, tt.want)
			}
		})
	}
}

func TestOwnedDescendantsKeepOwnershipOnceTheParentIsGone(t *testing.T) {
	t.Parallel()

	owned := ownedOverGroups(2)
	owned.observe([]processMember{
		{pid: 2, pgid: 2, ppid: 1},
		{pid: 3, pgid: 3, ppid: 2},
	})

	survivors := owned.observe([]processMember{{pid: 3, pgid: 3, ppid: 1}})

	if len(survivors) != 1 || survivors[0].pid != 3 {
		t.Errorf("observe(...) after the launch exited = %+v, want the detached descendant still owned", survivors)
	}
}

func TestOwnedDescendantsDropAProcessThatLeftTheTable(t *testing.T) {
	t.Parallel()

	owned := ownedOverGroups(2)
	owned.observe([]processMember{
		{pid: 2, pgid: 2, ppid: 1},
		{pid: 3, pgid: 3, ppid: 2},
	})
	owned.observe(nil)

	survivors := owned.observe([]processMember{{pid: 3, pgid: 3, ppid: 1}})

	if len(survivors) != 0 {
		t.Errorf("observe(...) = %+v, want nothing owned: process 3 had left the process table, so the id is no longer this run's", survivors)
	}
}

// The kernel writes an interpreter's name into argv, so group membership must
// be decided by the launch's process identity, not by name.
func TestInduceWorkspaceSecurityGradesAnInterpretedLaunch(t *testing.T) {
	t.Parallel()

	pgid, commandPath := startInterpretedLiveGroup(t)
	tracker := &groupTracker{}
	tracker.register(pgid)
	coords := Coordinates{CommandPath: commandPath, Profile: qualification.RuntimeProfile{ProjectConfigPaths: []string{".mcp.json"}}}
	fixture := &sharedFixture{workspaceRoot: t.TempDir(), tracker: tracker}

	obs := induceWorkspaceSecurity(t, coords, fixture)

	if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
		t.Errorf("induceWorkspaceSecurity(...) = %+v, want grade %q and outcome %q: the launch is the one the run started, whatever name its interpreter left in the process table", obs, qualification.GradeUsable, qualification.OutcomePass)
	}
}

const interpreterLinkName = "node"

func startInterpretedLiveGroup(t *testing.T) (pgid int, commandPath string) {
	t.Helper()

	dir := t.TempDir()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell to stand in for an interpreter: %v", err)
	}
	interpreter := filepath.Join(dir, interpreterLinkName)
	if err := os.Symlink(shell, interpreter); err != nil {
		t.Fatalf("link the interpreter: %v", err)
	}
	script := filepath.Join(dir, "sample-runtime")
	if len(interpreter) > 100 {
		t.Skipf("interpreter path %q is too long for a shebang line", interpreter)
	}
	if err := os.WriteFile(script, []byte("#!"+interpreter+"\nwhile :; do sleep 30; done\n"), 0o700); err != nil { //nolint:gosec // an executable fixture this test owns under its own temp directory
		t.Fatalf("write the interpreted runtime: %v", err)
	}

	cmd := exec.Command(script) //nolint:gosec // script is a fixture this test built under its own temp directory
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the interpreted launch: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	awaitInterpretedBasename(t, cmd.Process.Pid)
	return cmd.Process.Pid, script
}

func awaitInterpretedBasename(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot, err := psSnapshot()
		if err != nil {
			t.Fatalf("process-table query: %v", err)
		}
		for _, m := range snapshot {
			if m.pid != pid {
				continue
			}
			if m.basename == interpreterLinkName {
				return
			}
			t.Fatalf("the launched process reports basename %q, want %q: the control no longer reproduces an interpreted launch", m.basename, interpreterLinkName)
		}
		if time.Now().After(deadline) {
			t.Fatal("the launched process never appeared in the process table")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInduceProcessCleanupReportsADetachedReparentedDescendant(t *testing.T) {
	t.Parallel()

	launchPID, descendantPID := startDetachedDescendantLeak(t)
	adapter := &stubAdapter{stop: func() error {
		if err := signalProcessGroup(launchPID, syscall.SIGKILL); err != nil {
			return err
		}
		awaitReparentedDescendant(t, descendantPID, launchPID)
		return nil
	}}
	fixture := &sharedFixture{tracker: &groupTracker{}}
	if err := fixture.registerSession(adapter, domain.Session{ID: "induction", AgentPID: strconv.Itoa(launchPID)}); err != nil {
		t.Fatalf("registerSession(...) error = %v, want nil", err)
	}

	obs := induceProcessCleanup(fixture)

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation for a descendant this run left running outside every process group it registered", obs)
	}
	awaitProcessMember(t, descendantPID, "leave the process table under the drain", func(_ processMember, present bool) bool { return !present })
}

type detachedProbeLeakParams struct {
	Probe   string
	Receipt string
}

const detachedProbeLeakScenario = "detached-probe-leak"

// runDetachedProbeLeak starts a program in its own process group and keeps
// running, so the only link between it and the registered group is the parent.
func runDetachedProbeLeak(_ []string, params detachedProbeLeakParams) int {
	child := exec.Command(params.Probe) //nolint:gosec // the probe is a fixture the calling test built under its own temp directory
	procutil.SetProcessGroup(child)
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "detached-probe-leak: start %s: %v\n", params.Probe, err)
		return 1
	}
	if err := os.WriteFile(params.Receipt, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "detached-probe-leak: write the receipt: %v\n", err)
		_ = child.Process.Kill()
		return 1
	}
	agenttest.Hang()
	return 0
}

const lateDetachedChildScenario = "late-detached-child"

// lateDetachedChildDelay outlives two of a launch's readings, so the child
// started after it is born later than the last reading that launch takes.
const lateDetachedChildDelay = 2*launchOwnershipInterval + 200*time.Millisecond

// runLateDetachedChild serves longer than the reading interval, then starts
// one program in its own process group and exits, so no reading from outside
// can have seen it while a parent still linked it.
func runLateDetachedChild(_ []string, params detachedProbeLeakParams) int {
	time.Sleep(lateDetachedChildDelay)
	child := exec.Command(params.Probe) //nolint:gosec // the probe is a fixture the calling test built under its own temp directory
	procutil.SetProcessGroup(child)
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "late-detached-child: start %s: %v\n", params.Probe, err)
		return 1
	}
	if err := os.WriteFile(params.Receipt, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "late-detached-child: write the receipt: %v\n", err)
		_ = child.Process.Kill()
		return 1
	}
	return 0
}

func init() {
	probeScenarios[detachedProbeLeakScenario] = agenttest.Typed(runDetachedProbeLeak)
	probeScenarios[lateDetachedChildScenario] = agenttest.Typed(runLateDetachedChild)
}

func TestInduceProcessCleanupReportsADescendantOrphanedBeforeTheReading(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	receipt := filepath.Join(dir, "descendant.pid")
	probe := agenttest.WriteScript(t, dir, "detached-probe", "while true; do sleep 0.1; done\n")
	command := agenttest.FakeRuntime(t, dir, "detaching-runtime", detachedProbeLeakScenario, detachedProbeLeakParams{Probe: probe, Receipt: receipt})

	fixture := semanticFixture(t)
	launch, err := startBoundedLaunch(command, nil, dir, nil, fixture.ownership())
	if err != nil {
		t.Fatalf("startBoundedLaunch(...) error = %v, want nil", err)
	}
	t.Cleanup(launch.cancel)
	// Registered before the launch can detach anything, so a later failure
	// still frees the descendant.
	t.Cleanup(func() { killReportedPID(receipt) })

	if !awaitProbeExecution(fixture.ownership(), probe, launch.pgid, time.Now().Add(processTableBound)) {
		t.Fatal("the probe was never observed running under this launch, so the control never reached the one reading that can prove ownership")
	}
	descendantPID := awaitReportedPID(t, receipt)
	requireDetachedDescendant(t, launch.pgid, descendantPID)

	if _, _, ended := launch.terminate(); !ended {
		t.Fatal("the launch did not end within its own drain bound after its group was taken down")
	}
	awaitReparentedDescendant(t, descendantPID, launch.pgid)

	obs := induceProcessCleanup(fixture)

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation for a descendant the run orphaned before the reading was taken", obs)
	}
	awaitProcessMember(t, descendantPID, "leave the process table under the drain", func(_ processMember, present bool) bool { return !present })
}

func TestInduceProcessCleanupReportsADescendantOfAPlainNativeLaunch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	receipt := filepath.Join(dir, "descendant.pid")
	probe := agenttest.WriteScript(t, dir, "detached-probe", "while true; do sleep 0.1; done\n")
	command := agenttest.FakeRuntime(t, dir, "detaching-runtime", detachedProbeLeakScenario, detachedProbeLeakParams{Probe: probe, Receipt: receipt})

	fixture := semanticFixture(t)
	launch, err := startBoundedLaunch(command, nil, dir, nil, fixture.ownership())
	if err != nil {
		t.Fatalf("startBoundedLaunch(...) error = %v, want nil", err)
	}
	t.Cleanup(launch.cancel)
	t.Cleanup(func() { killReportedPID(receipt) })

	descendantPID := awaitReportedPID(t, receipt)
	requireDetachedDescendant(t, launch.pgid, descendantPID)
	awaitLedgerOwnership(t, fixture.ownership(), descendantPID)

	if _, _, ended := launch.terminate(); !ended {
		t.Fatal("the launch did not end within its own drain bound after its group was taken down")
	}
	awaitReparentedDescendant(t, descendantPID, launch.pgid)

	obs := induceProcessCleanup(fixture)

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation for a descendant of a launch no probe wait watched", obs)
	}
	awaitProcessMember(t, descendantPID, "leave the process table under the drain", func(_ processMember, present bool) bool { return !present })
}

// A process-table read is an observation, not a proof, so for a program this
// run authored, attribution rests on what that program records about its own
// children on the way out.
func TestInduceProcessCleanupReportsAChildStartedAfterTheLastReading(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	receipt := filepath.Join(dir, "descendant.pid")
	probe := agenttest.WriteScript(t, dir, "detached-probe", "while true; do sleep 0.1; done\n")
	command := agenttest.FakeRuntime(t, dir, "late-detaching-runtime", lateDetachedChildScenario, detachedProbeLeakParams{Probe: probe, Receipt: receipt})
	// Registered before the runtime can exist, so a later failure still frees
	// whatever it detached.
	t.Cleanup(func() { killReportedPID(receipt) })

	fixture := semanticFixture(t)
	launch, err := startBoundedLaunch(command, nil, dir, nil, fixture.ownership())
	if err != nil {
		t.Fatalf("startBoundedLaunch(...) error = %v, want nil", err)
	}
	t.Cleanup(launch.cancel)

	if _, _, ended := launch.await(lateDetachedChildDelay + processTableBound); !ended {
		t.Fatal("the runtime never exited, so the control never reached the reading a vanished parent is read under")
	}
	descendantPID := awaitReportedPID(t, receipt)
	member := awaitProcessMember(t, descendantPID, "appear in the process table", func(_ processMember, present bool) bool { return present })
	if member.pgid != descendantPID {
		t.Fatalf("the detached descendant runs in process group %d, want the group %d it leads itself", member.pgid, descendantPID)
	}
	if member.pgid == launch.pgid {
		t.Fatalf("the detached descendant runs in the launch's own process group %d, want a group the run never registered", launch.pgid)
	}
	if member.ppid == launch.pgid {
		t.Fatalf("the detached descendant's parent is still the launch %d, want it orphaned before the reading", launch.pgid)
	}

	obs := induceProcessCleanup(fixture)

	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceProcessCleanup(...) = %+v, want a negative observation for a descendant started after the last reading its launch took", obs)
	}
	awaitProcessMember(t, descendantPID, "leave the process table under the drain", func(_ processMember, present bool) bool { return !present })
}

func awaitLedgerOwnership(t *testing.T, owned *ownedDescendants, pid int) {
	t.Helper()

	deadline := time.Now().Add(processTableBound)
	for {
		owned.mu.Lock()
		held := owned.owned[pid]
		owned.mu.Unlock()
		if held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ledger never recorded process %d while the launch that started it was running, within %s", pid, processTableBound)
		}
		time.Sleep(processTablePollInterval)
	}
}

// killReportedPID frees the reported pid, addressed by id alone so nothing
// this control did not start is reached.
func killReportedPID(receipt string) {
	reported, err := os.ReadFile(receipt)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(reported)))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

const detachedLeakHelperEnv = "PROBE_DETACHED_LEAK_HELPER_PROCESS"

const detachedLeakReportEnv = "PROBE_DETACHED_LEAK_REPORT_FILE"

const detachedLeakSleeperEnv = "PROBE_DETACHED_LEAK_SLEEPER"

// detachedLeakLifetime bounds both halves of the control, so a failure before
// anything kills them still leaves nothing running longer than this.
const detachedLeakLifetime = 2 * time.Minute

// TestDetachedLeakHelperProcess is the subprocess launch half of the
// detached-descendant control: it detaches a sleeper into a session of its
// own, reports its pid, and waits to be killed, the shape a runtime's shell
// tool leaves behind.
func TestDetachedLeakHelperProcess(t *testing.T) {
	if os.Getenv(detachedLeakHelperEnv) != "1" {
		t.Skip("the launch half of the detached-descendant control, started only by that control")
	}

	descendant := exec.Command(os.Getenv(detachedLeakSleeperEnv), strconv.Itoa(int(detachedLeakLifetime.Seconds()))) //nolint:gosec // the control passes the path of the sleeper it resolved itself
	descendant.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := descendant.Start(); err != nil {
		t.Fatalf("start the detached descendant: %v", err)
	}
	if err := os.WriteFile(os.Getenv(detachedLeakReportEnv), []byte(strconv.Itoa(descendant.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatalf("report the detached descendant: %v", err)
	}
	time.Sleep(detachedLeakLifetime)
}

func startDetachedDescendantLeak(t *testing.T) (launchPID, descendantPID int) {
	t.Helper()

	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleeper to detach: %v", err)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedLeakHelperProcess$") //nolint:gosec // re-invokes this package's own compiled test binary
	// The launch half is killed, not returned from, so its own test main never
	// cleans up its temp files; TMPDIR puts them where this test's cleanup does.
	cmd.Env = append(os.Environ(),
		"TMPDIR="+dir,
		detachedLeakHelperEnv+"=1",
		detachedLeakReportEnv+"="+filepath.Join(dir, "descendant.pid"),
		detachedLeakSleeperEnv+"="+sleeper,
	)
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the launch: %v", err)
	}
	launchPID = cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = signalProcessGroup(launchPID, syscall.SIGKILL)
		<-done
	})

	descendantPID = awaitReportedPID(t, filepath.Join(dir, "descendant.pid"))
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	requireDetachedDescendant(t, launchPID, descendantPID)
	return launchPID, descendantPID
}

// awaitReportedPID reads the pid the launch half wrote to path. The trailing
// newline distinguishes a short read from a complete one.
func awaitReportedPID(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(processTableBound)
	for {
		reported, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(reported), "\n") {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(reported)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launch never reported a detached descendant within %s", processTableBound)
		}
		time.Sleep(processTablePollInterval)
	}
}

// requireDetachedDescendant asserts the descendant leads its own group and is
// a child of the launch while it lives, so the control is not grading an
// ordinary group member.
func requireDetachedDescendant(t *testing.T, launchPID, descendantPID int) {
	t.Helper()

	member := awaitProcessMember(t, descendantPID, "appear in the process table", func(_ processMember, present bool) bool { return present })
	if member.pgid != descendantPID {
		t.Fatalf("the detached descendant runs in process group %d, want the group %d it leads itself", member.pgid, descendantPID)
	}
	if member.pgid == launchPID {
		t.Fatalf("the detached descendant runs in the launch's own process group %d, want a group the run never registered", launchPID)
	}
	if member.ppid != launchPID {
		t.Fatalf("the detached descendant's parent is %d, want the launch %d: ownership is provable only through that link", member.ppid, launchPID)
	}
}

// awaitReparentedDescendant waits for the descendant to lose the parent the
// shutdown killed. Waiting inside the session's stop puts the reparenting
// before the observation's reading, so the control grades the case it names
// rather than racing it.
func awaitReparentedDescendant(t *testing.T, descendantPID, launchPID int) {
	t.Helper()

	awaitProcessMember(t, descendantPID, "lose the launch that started it as its parent", func(member processMember, present bool) bool {
		if !present {
			t.Fatalf("the detached descendant %d left the process table with its parent, want it to outlive the launch", descendantPID)
		}
		return member.ppid != launchPID
	})
}

const processTableBound = 10 * time.Second

const processTablePollInterval = 10 * time.Millisecond

// awaitProcessMember polls until cond holds for pid's row and returns it. cond
// receives the zero row and false when no row belongs to pid; what names the
// condition in the timeout message.
func awaitProcessMember(t *testing.T, pid int, what string, cond func(member processMember, present bool) bool) processMember {
	t.Helper()

	deadline := time.Now().Add(processTableBound)
	for {
		snapshot, err := psSnapshot()
		if err != nil {
			t.Fatalf("process-table query: %v", err)
		}
		var member processMember
		var present bool
		for _, candidate := range snapshot {
			if candidate.pid == pid {
				member, present = candidate, true
				break
			}
		}
		if cond(member, present) {
			return member
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not %s within %s", pid, what, processTableBound)
		}
		time.Sleep(processTablePollInterval)
	}
}
