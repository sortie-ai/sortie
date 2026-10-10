//go:build unix

package fakemodel

import (
	"os/exec"
	"syscall"
	"testing"
)

func startGroupMember(t *testing.T, group int) *exec.Cmd {
	t.Helper()

	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("skipping: no sleeper to start: %v", err)
	}
	cmd := exec.Command(sleeper, "60") //nolint:gosec // the program is the sleeper just resolved
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", sleeper, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func reap(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func TestRemainingRuntimeProbesTheWholeProcessGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reapLeader bool
		reapMember bool
		wantEmpty  bool
	}{
		{name: "both alive"},
		{name: "member alive after the leader is reaped", reapLeader: true},
		{name: "both reaped", reapLeader: true, reapMember: true, wantEmpty: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			leader := startGroupMember(t, 0)
			member := startGroupMember(t, leader.Process.Pid)
			if tt.reapLeader {
				reap(leader)
			}
			if tt.reapMember {
				reap(member)
			}

			got := remainingRuntime(leader.Process.Pid)

			if (got == "") != tt.wantEmpty {
				t.Errorf("remainingRuntime(%d) = %q, want empty = %t", leader.Process.Pid, got, tt.wantEmpty)
			}
		})
	}
}
