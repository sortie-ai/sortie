package procutil

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
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
	SetGroupCancel(cmd, DefaultStopGrace)

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

// TestSetGroupCancel_GraceResolution asserts WaitDelay is set to
// the grace passed when it is positive, and falls back to
// DefaultStopGrace when it is non-positive, so a cancelled turn never
// loses its escalation to a force kill by inheriting os/exec's
// zero-WaitDelay "no limit" reading.
func TestSetGroupCancel_GraceResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		grace time.Duration
		want  time.Duration
	}{
		{"PositiveGraceHonored", 250 * time.Millisecond, 250 * time.Millisecond},
		{"ZeroGraceFallsBackToDefault", 0, DefaultStopGrace},
		{"NegativeGraceFallsBackToDefault", -5 * time.Second, DefaultStopGrace},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			cmd := exec.CommandContext(ctx, os.Args[0])
			SetGroupCancel(cmd, tt.grace)

			if cmd.WaitDelay != tt.want {
				t.Errorf("SetGroupCancel(cmd, %v) WaitDelay = %v, want %v", tt.grace, cmd.WaitDelay, tt.want)
			}
		})
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
	SetGroupCancel(cmd, DefaultStopGrace)

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
