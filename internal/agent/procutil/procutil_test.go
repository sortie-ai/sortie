package procutil

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
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

func TestExtractExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subtests require /bin/sh")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("subtests require /bin/sh and POSIX signals")
	}
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

func TestStderrCollector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantLines []string
	}{
		{
			name:      "empty reader returns nil",
			input:     "",
			wantLines: nil,
		},
		{
			name:      "single line collected",
			input:     "startup failed\n",
			wantLines: []string{"startup failed"},
		},
		{
			name:      "multiple lines collected in order",
			input:     "error one\nerror two\nerror three\n",
			wantLines: []string{"error one", "error two", "error three"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := NewStderrCollector(strings.NewReader(tt.input), slog.Default())
			got := c.Lines()
			if len(got) != len(tt.wantLines) {
				t.Fatalf("Lines() = %v, want %v", got, tt.wantLines)
			}
			for i, want := range tt.wantLines {
				if got[i] != want {
					t.Errorf("Lines()[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestStderrCollector_LogsDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := NewStderrCollector(strings.NewReader("worker error\nnetwork timeout\n"), logger)
	_ = c.Lines()

	output := buf.String()
	for _, line := range []string{"worker error", "network timeout"} {
		if !strings.Contains(output, line) {
			t.Errorf("StderrCollector log missing %q; output: %s", line, output)
		}
	}
	if strings.Contains(output, "WARN") {
		t.Errorf("StderrCollector logged at WARN during drain; want DEBUG only; output: %s", output)
	}
}

func TestStderrCollector_NilLogger(t *testing.T) {
	t.Parallel()
	c := NewStderrCollector(strings.NewReader("some output\n"), nil)
	got := c.Lines()
	if len(got) != 1 || got[0] != "some output" {
		t.Errorf("Lines() = %v, want [\"some output\"]", got)
	}
}

func TestStderrCollector_ScannerError(t *testing.T) {
	t.Parallel()
	longLine := strings.Repeat("x", bufio.MaxScanTokenSize+1)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := NewStderrCollector(strings.NewReader(longLine), logger)
	_ = c.Lines()

	if !strings.Contains(buf.String(), "agent stderr drain failed") {
		t.Errorf("StderrCollector did not log scanner error; output = %q", buf.String())
	}
}

func TestStderrCollector_WarnLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantWarn []string
	}{
		{
			name:     "empty collector produces no WARN",
			input:    "",
			wantWarn: nil,
		},
		{
			name:     "single line re-emitted at WARN",
			input:    "startup rejected: no license\n",
			wantWarn: []string{"startup rejected: no license"},
		},
		{
			name:     "multiple lines all re-emitted at WARN",
			input:    "error one\nerror two\n",
			wantWarn: []string{"error one", "error two"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
			logger := slog.New(handler)

			c := NewStderrCollector(strings.NewReader(tt.input), slog.Default())
			c.WarnLines(logger)

			output := buf.String()
			for _, want := range tt.wantWarn {
				if !strings.Contains(output, want) {
					t.Errorf("WarnLines() output missing %q; got: %s", want, output)
				}
			}
			if len(tt.wantWarn) == 0 && output != "" {
				t.Errorf("WarnLines() produced output for empty collector; got: %s", output)
			}
		})
	}
}

func TestStderrCollector_WarnLines_NilLogger(t *testing.T) {
	t.Parallel()

	c := NewStderrCollector(strings.NewReader("test line\n"), slog.Default())
	// Must not panic when logger is nil — falls back to slog.Default().
	c.WarnLines(nil)

	// No assertion on output — the test verifies the nil guard does not panic.
}

func TestEmitWarnLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lines    []string
		wantWarn []string
	}{
		{
			name:     "nil slice produces no WARN",
			lines:    nil,
			wantWarn: nil,
		},
		{
			name:     "empty slice produces no WARN",
			lines:    []string{},
			wantWarn: nil,
		},
		{
			name:     "single line re-emitted at WARN",
			lines:    []string{"startup rejected: no license"},
			wantWarn: []string{"startup rejected: no license"},
		},
		{
			name:     "multiple lines all re-emitted at WARN",
			lines:    []string{"error one", "error two"},
			wantWarn: []string{"error one", "error two"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
			logger := slog.New(handler)

			EmitWarnLines(tt.lines, logger)

			output := buf.String()
			for _, want := range tt.wantWarn {
				if !strings.Contains(output, want) {
					t.Errorf("EmitWarnLines() output missing %q; got: %s", want, output)
				}
			}
			if len(tt.wantWarn) == 0 && output != "" {
				t.Errorf("EmitWarnLines() produced output for empty input; got: %s", output)
			}
		})
	}
}

func TestEmitWarnLines_NilLogger(t *testing.T) {
	t.Parallel()

	// Must not panic when logger is nil — falls back to slog.Default().
	EmitWarnLines([]string{"test line"}, nil)
}
