//go:build unix

package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestBoundedLaunchEndsWhileADescendantHoldsTheStreams(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := agenttest.WriteScript(t, dir, "leaves-a-holder",
		"sh -c 'sleep 120' &\necho parent-done\nexit 0\n")

	type launchResult struct {
		output string
		err    error
	}
	done := make(chan launchResult, 1)
	go func() {
		output, err := launchNativeProbe(t, script, nil, dir, nil, newOwnedDescendants(&groupTracker{}))
		done <- launchResult{output: output, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Errorf("launchNativeProbe(...) error = %v, want nil: the direct child exited cleanly", got.err)
		}
		if !strings.Contains(got.output, "parent-done") {
			t.Errorf("launchNativeProbe(...) output = %q, want it to carry the direct child's own line", got.output)
		}
	case <-time.After(2 * nativeDrainBound):
		t.Fatal("launchNativeProbe(...) did not return while a descendant held the inherited output streams")
	}
}

func TestPSSnapshotIsBounded(t *testing.T) {
	t.Parallel()

	if psSnapshotBound <= 0 {
		t.Fatalf("psSnapshotBound = %s, want a positive bound", psSnapshotBound)
	}

	members, err := psSnapshot()
	if err != nil {
		t.Fatalf("psSnapshot() error = %v, want nil", err)
	}
	if len(members) == 0 {
		t.Error("psSnapshot() returned no member, want at least this process")
	}
}
