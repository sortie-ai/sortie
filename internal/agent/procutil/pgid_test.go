package procutil

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// TestSetGroupCancel_Wiring covers what is observable without starting a
// process: the command joins its own process group, its cancellation
// function is no longer os/exec's, and the escalation to a force kill is
// bounded by the shared grace period.
//
// The cancellation function is compared against the default one rather
// than against nil. exec.CommandContext installs its own Cancel, which
// kills the direct child alone, so a nil check would pass on a command
// this helper never touched and prove nothing.
func TestSetGroupCancel_Wiring(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	defaultCancel := reflect.ValueOf(exec.CommandContext(ctx, os.Args[0]).Cancel).Pointer()

	cmd := exec.CommandContext(ctx, os.Args[0])
	SetGroupCancel(cmd)

	if cmd.SysProcAttr == nil {
		t.Error("SetGroupCancel() SysProcAttr = nil, want non-nil (the command must join its own process group)")
	}
	if got := reflect.ValueOf(cmd.Cancel).Pointer(); got == defaultCancel {
		t.Error("SetGroupCancel() Cancel = the os/exec default, want a group-signalling replacement")
	}
	if cmd.WaitDelay != DefaultStopGrace {
		t.Errorf("SetGroupCancel() WaitDelay = %v, want %v", cmd.WaitDelay, DefaultStopGrace)
	}
}

// TestSetGroupCancel_RequiresCommandContext pins the precondition the
// godoc states: os/exec refuses to start a command that carries a Cancel
// function but was built without a context, so a launcher that reaches
// for this helper on an exec.Command fails loudly at Start rather than
// silently losing group teardown.
func TestSetGroupCancel_RequiresCommandContext(t *testing.T) {
	t.Parallel()

	// os.Args[0] is the test binary: a path that always resolves, so
	// Start reaches the Cancel precondition instead of failing earlier
	// on lookup. It is never executed, because that check returns first.
	cmd := exec.Command(os.Args[0])
	SetGroupCancel(cmd)

	err := cmd.Start()
	if err == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("cmd.Start() = nil, want an error for a Cancel function without a context")
	}
	if !strings.Contains(err.Error(), "CommandContext") {
		t.Errorf("cmd.Start() = %v, want an error naming CommandContext", err)
	}
}
