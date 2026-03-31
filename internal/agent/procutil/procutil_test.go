package procutil

import (
	"bytes"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

// signalledErr starts a blocking subprocess, kills it with SIGKILL, and
// returns the resulting *exec.ExitError. Used to produce a signal-terminated
// error for WasSignaled tests.
func signalledErr(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("cat")
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

func TestExtractExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		makeErr func(t *testing.T) error
		want    int
	}{
		{
			name:    "nil error returns 0",
			makeErr: func(_ *testing.T) error { return nil },
			want:    0,
		},
		{
			name: "ExitError code 1",
			makeErr: func(t *testing.T) error {
				t.Helper()
				return exec.Command("/bin/sh", "-c", "exit 1").Run()
			},
			want: 1,
		},
		{
			name: "ExitError code 42",
			makeErr: func(t *testing.T) error {
				t.Helper()
				return exec.Command("/bin/sh", "-c", "exit 42").Run()
			},
			want: 42,
		},
		{
			name:    "non-ExitError returns -1",
			makeErr: func(_ *testing.T) error { return errors.New("something went wrong") },
			want:    -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractExitCode(tt.makeErr(t))
			if got != tt.want {
				t.Errorf("ExtractExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
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

func TestDrainStderr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantLines []string
	}{
		{
			name:      "empty input logs nothing",
			input:     "",
			wantLines: nil,
		},
		{
			name:      "single line is logged",
			input:     "start failed\n",
			wantLines: []string{"start failed"},
		},
		{
			name:      "multiple lines are all logged",
			input:     "line one\nline two\nline three\n",
			wantLines: []string{"line one", "line two", "line three"},
		},
		{
			name:      "session ID in line is logged verbatim",
			input:     "Session ID: abc-def-123\nconnected\n",
			wantLines: []string{"Session ID: abc-def-123", "connected"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			DrainStderr(strings.NewReader(tt.input), logger)

			output := buf.String()
			for _, want := range tt.wantLines {
				if !strings.Contains(output, want) {
					t.Errorf("DrainStderr log output does not contain %q; got: %s", want, output)
				}
			}
			if len(tt.wantLines) == 0 && output != "" {
				t.Errorf("DrainStderr log output = %q, want empty for empty input", output)
			}
		})
	}
}
