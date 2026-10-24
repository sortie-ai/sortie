//go:build unix

package probe

import (
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

func startStrangerGroup(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("sleep", "60")
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the unrelated process group: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd.Process.Pid
}

func TestReapedGroupNumberLeavesStrangerUntouched(t *testing.T) {
	t.Parallel()

	stranger := startStrangerGroup(t)
	fixture := &sharedFixture{tracker: &groupTracker{}}
	owned := fixture.ownership()
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("exec.LookPath(%q) = %v, want nil", "true", err)
	}
	launch, err := startBoundedLaunch(truePath, nil, t.TempDir(), nil, owned)
	if err != nil {
		t.Fatalf("startBoundedLaunch(%q) = %v, want nil", truePath, err)
	}
	if _, _, ended := launch.await(nativeProbeBound); !ended {
		t.Fatalf("launch of %q did not end within %s", truePath, nativeProbeBound)
	}
	session := domain.Session{ID: "reaped", AgentPID: strconv.Itoa(stranger)}

	record, err := fixture.registerSession(&stubAdapter{}, session)
	if err != nil {
		t.Fatalf("registerSession(%q) = %v, want nil", session.AgentPID, err)
	}
	requireTornDown(t, record)
	obs := observeProcessCleanup(fixture, owned)
	drainOwnedProcesses(owned)

	if obs.Grade != evidence.GradeUsable || obs.Outcome != evidence.OutcomePass {
		t.Errorf("observeProcessCleanup(...) = %+v, want grade %q and outcome %q", obs, evidence.GradeUsable, evidence.OutcomePass)
	}
	if err := syscall.Kill(stranger, 0); err != nil {
		t.Errorf("syscall.Kill(%d, 0) = %v, want nil: the drain must leave an unrelated process alone", stranger, err)
	}
	_, held, err := owned.readHeld()
	if err != nil {
		t.Fatalf("readHeld() = %v, want nil", err)
	}
	if slices.Contains(held, launch.group.PID()) {
		t.Errorf("readHeld() numbers = %v, want none equal to the reaped launch's %d", held, launch.group.PID())
	}
}
