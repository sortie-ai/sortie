package procutil

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

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
	// Feed a line that exceeds the configured 128-byte scanner max so the
	// scanner returns ErrTooLong while draining stderr.
	longLine := strings.Repeat("x", 129)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := NewStderrCollector(strings.NewReader(longLine), logger, WithScannerMax(128))
	_ = c.Lines()

	if !strings.Contains(buf.String(), "agent stderr drain failed") {
		t.Errorf("StderrCollector did not log scanner error; output = %q", buf.String())
	}
}

func TestStderrCollector_LargeLineCollected(t *testing.T) {
	t.Parallel()

	bigLine := strings.Repeat("x", 100*1024)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := NewStderrCollector(strings.NewReader(bigLine+"\n"), logger)
	got := c.Lines()

	if len(got) != 1 {
		t.Fatalf("Lines() returned %d lines, want 1", len(got))
	}
	if got[0] != bigLine {
		t.Errorf("Lines()[0] length = %d, want %d", len(got[0]), len(bigLine))
	}
	if strings.Contains(buf.String(), "agent stderr drain failed") {
		t.Error("StderrCollector logged scanner error for large line within default scanner max")
	}
}

func TestStderrCollector_LineCap(t *testing.T) {
	t.Parallel()

	makeInput := func(n int) (string, []string) {
		ls := make([]string, n)
		var sb strings.Builder
		for i := range n {
			s := fmt.Sprintf("line%d", i+1)
			ls[i] = s
			sb.WriteString(s)
			sb.WriteByte('\n')
		}
		return sb.String(), ls
	}

	tests := []struct {
		name       string
		n          int
		wantLen    int
		wantDrop   int
		wantMarker bool
		wantFirst  []string
		wantLast   []string
	}{
		{
			name:       "exactly at cap",
			n:          10,
			wantLen:    10,
			wantDrop:   0,
			wantMarker: false,
			wantFirst:  []string{"line1", "line2", "line3", "line4", "line5"},
			wantLast:   []string{"line6", "line7", "line8", "line9", "line10"},
		},
		{
			name:       "one over cap",
			n:          11,
			wantLen:    11,
			wantDrop:   1,
			wantMarker: true,
			wantFirst:  []string{"line1", "line2", "line3", "line4", "line5"},
			wantLast:   []string{"line7", "line8", "line9", "line10", "line11"},
		},
		{
			name:       "large overage",
			n:          25,
			wantLen:    11,
			wantDrop:   15,
			wantMarker: true,
			wantFirst:  []string{"line1", "line2", "line3", "line4", "line5"},
			wantLast:   []string{"line21", "line22", "line23", "line24", "line25"},
		},
		{
			name:       "all fit in head",
			n:          3,
			wantLen:    3,
			wantDrop:   0,
			wantMarker: false,
			wantFirst:  []string{"line1", "line2", "line3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input, _ := makeInput(tt.n)
			c := NewStderrCollector(strings.NewReader(input), slog.Default(), WithMaxLines(10))
			got := c.Lines()
			dropped := c.Dropped()

			if len(got) != tt.wantLen {
				t.Fatalf("len(Lines()) = %d, want %d; lines = %v", len(got), tt.wantLen, got)
			}
			if dropped != tt.wantDrop {
				t.Errorf("Dropped() = %d, want %d", dropped, tt.wantDrop)
			}
			for i, want := range tt.wantFirst {
				if i >= len(got) {
					break
				}
				if got[i] != want {
					t.Errorf("Lines()[%d] = %q, want %q", i, got[i], want)
				}
			}
			for i, want := range tt.wantLast {
				idx := len(got) - len(tt.wantLast) + i
				if got[idx] != want {
					t.Errorf("Lines()[%d] = %q, want %q", idx, got[idx], want)
				}
			}
			wantMarkerStr := fmt.Sprintf(droppedMarkerFmt, tt.wantDrop)
			hasMarker := false
			for _, line := range got {
				if line == wantMarkerStr {
					hasMarker = true
					break
				}
			}
			if hasMarker != tt.wantMarker {
				t.Errorf("marker %q present = %v, want %v", wantMarkerStr, hasMarker, tt.wantMarker)
			}
		})
	}
}

func TestStderrCollector_WithScannerMax(t *testing.T) {
	t.Parallel()

	// A line of 128 KiB + 1 byte exceeds the 128 KiB scanner limit but
	// would be collected fine under the default 10 MiB scanner limit.
	const customMax = 128 * 1024
	bigLine := strings.Repeat("y", customMax+1)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c := NewStderrCollector(strings.NewReader(bigLine+"\n"), logger, WithScannerMax(customMax))
	got := c.Lines()

	if !strings.Contains(buf.String(), "agent stderr drain failed") {
		t.Error("StderrCollector did not log scanner error for line exceeding WithScannerMax(128 KiB)")
	}
	for _, line := range got {
		if line == bigLine {
			t.Error("Lines() contains the oversized line that exceeded WithScannerMax(128 KiB)")
		}
	}
}

func TestStderrCollector_ByteBudget(t *testing.T) {
	t.Parallel()

	t.Run("under budget", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("a", 20)
		var sb strings.Builder
		for range 10 {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		c := NewStderrCollector(strings.NewReader(sb.String()), slog.Default(),
			WithMaxBytes(256), WithMaxLines(100))
		got := c.Lines()
		if len(got) != 10 {
			t.Errorf("Lines() count = %d, want 10", len(got))
		}
		if d := c.Dropped(); d != 0 {
			t.Errorf("Dropped() = %d, want 0", d)
		}
	})

	t.Run("over budget", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("b", 20)
		var sb strings.Builder
		for range 20 {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		c := NewStderrCollector(strings.NewReader(sb.String()), slog.Default(),
			WithMaxBytes(256), WithMaxLines(100))
		got := c.Lines()
		if d := c.Dropped(); d <= 0 {
			t.Errorf("Dropped() = %d, want > 0", d)
		}
		var total int
		for _, l := range got {
			// Exclude the synthetic drop-marker line from the byte sum.
			if !strings.HasPrefix(l, "...") {
				total += len(l)
			}
		}
		if total > 256 {
			t.Errorf("retained bytes = %d, want ≤ 256", total)
		}
	})

	t.Run("head partially filled", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("c", 100)
		var sb strings.Builder
		for range 5 {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		c := NewStderrCollector(strings.NewReader(sb.String()), slog.Default(),
			WithMaxBytes(256), WithMaxLines(100))
		got := c.Lines()
		if len(got) >= 5 {
			t.Errorf("Lines() count = %d, want < 5 (budget exhausted before all lines stored)", len(got))
		}
		if d := c.Dropped(); d <= 0 {
			t.Errorf("Dropped() = %d, want > 0", d)
		}
	})

	t.Run("ring reclamation", func(t *testing.T) {
		t.Parallel()
		headLine := strings.Repeat("h", 10)
		tailLine := strings.Repeat("t", 25)
		extraLine := strings.Repeat("e", 20)
		var sb strings.Builder
		for range 3 {
			sb.WriteString(headLine)
			sb.WriteByte('\n')
		}
		for range 3 {
			sb.WriteString(tailLine)
			sb.WriteByte('\n')
		}
		sb.WriteString(extraLine)
		sb.WriteByte('\n')
		c := NewStderrCollector(strings.NewReader(sb.String()), slog.Default(),
			WithMaxBytes(120), WithMaxLines(6))
		if d := c.Dropped(); d != 1 {
			t.Errorf("Dropped() = %d, want 1 (one tail eviction via byte reclamation)", d)
		}
	})
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
