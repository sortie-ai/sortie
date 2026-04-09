//go:build unix

package procutil

import (
	"errors"
	"os/exec"
	"testing"
)

// signalledErr starts a blocking subprocess, kills it with SIGKILL, and
// returns the resulting *exec.ExitError. Uses sleep(1) which blocks
// indefinitely without reading stdin, avoiding races where cat exits
// immediately on a closed stdin (e.g. in CI).
func signalledErr(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("signalledErr: Start: %v", err)
	}
	_ = cmd.Process.Kill()
	err := cmd.Wait()
	if err == nil {
		t.Fatal("signalledErr: expected error from killed process, got nil")
	}
	return err
}

func TestWasSignaled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		makeErr func(t *testing.T) error
		want    bool
	}{
		{
			name:    "nil error returns false",
			makeErr: func(_ *testing.T) error { return nil },
			want:    false,
		},
		{
			name:    "process killed by signal returns true",
			makeErr: signalledErr,
			want:    true,
		},
		{
			name: "normal exit returns false",
			makeErr: func(t *testing.T) error {
				t.Helper()
				return exec.Command("/bin/sh", "-c", "exit 1").Run()
			},
			want: false,
		},
		{
			name:    "non-ExitError returns false",
			makeErr: func(_ *testing.T) error { return errors.New("not an exit error") },
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := WasSignaled(tt.makeErr(t))
			if got != tt.want {
				t.Errorf("WasSignaled() = %v, want %v", got, tt.want)
			}
		})
	}
}
