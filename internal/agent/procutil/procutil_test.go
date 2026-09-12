package procutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

// TestStopGrace asserts a non-positive ms resolves to
// DefaultStopGrace, and a positive one resolves to the exact
// millisecond-to-duration conversion.
func TestStopGrace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"Zero", 0, DefaultStopGrace},
		{"NegativeOne", -1, DefaultStopGrace},
		{"LargeNegative", -60000, DefaultStopGrace},
		{"OneMillisecond", 1, time.Millisecond},
		{"OneSecond", 1000, time.Second},
		{"ExactBuiltInDefaultInMS", 5000, DefaultStopGrace},
		{"ArbitraryPositive", 1500, 1500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := StopGrace(tt.ms)
			if got != tt.want {
				t.Errorf("StopGrace(%d) = %v, want %v", tt.ms, got, tt.want)
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
			hasMarker := slices.Contains(got, wantMarkerStr)
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

	// Must not panic when logger is nil: falls back to slog.Default().
	EmitWarnLines([]string{"test line"}, nil)
}

// TestStderrCollector_DoneClosesOnlyAfterDrainComplete pins the contract
// documented on [StderrCollector.Done]: the channel it returns closes only
// once the drain goroutine has fully consumed the reader, so a caller that
// waits on Done before reading the underlying pipe further (as
// [exec.Cmd.Wait] does when it closes the pipe after reaping the child) is
// guaranteed every line the reader ever produced is already in Lines().
//
// The reader is an [io.Pipe] whose writer withholds its final line and the
// EOF-producing Close until the test releases them, so the assertion that
// Done has not yet fired is a genuine channel block rather than a timing
// guess.
func TestStderrCollector_DoneClosesOnlyAfterDrainComplete(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	c := NewStderrCollector(pr, slog.Default())

	if _, err := io.WriteString(pw, "first line\n"); err != nil {
		t.Fatalf("pw.Write(first line) = %v", err)
	}

	select {
	case <-c.Done():
		t.Fatal("Done() closed before the writer sent the final line and closed the pipe")
	case <-time.After(200 * time.Millisecond):
	}

	const finalLine = "final line released after the block"
	if _, err := io.WriteString(pw, finalLine+"\n"); err != nil {
		t.Fatalf("pw.Write(final line) = %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("pw.Close() = %v", err)
	}

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after the reader reached EOF")
	}

	got := c.Lines()
	want := []string{"first line", finalLine}
	if !slices.Equal(got, want) {
		t.Fatalf("Lines() after Done() closed = %v, want %v", got, want)
	}
}

// TestStderrCollector_WaitDoneHealthyDrain pins the healthy path: a drain
// that reaches EOF is never abandoned.
func TestStderrCollector_WaitDoneHealthyDrain(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c := NewStderrCollector(strings.NewReader("startup ok\n"), logger)

	start := time.Now()
	if !c.WaitDone(DefaultDrainGrace) {
		t.Fatalf("WaitDone(%v) = false, want true", DefaultDrainGrace)
	}
	if elapsed := time.Since(start); elapsed >= DefaultDrainGrace {
		t.Errorf("WaitDone(%v) took %v, want well under the bound", DefaultDrainGrace, elapsed)
	}

	want := []string{"startup ok"}
	if got := c.Lines(); !slices.Equal(got, want) {
		t.Errorf("Lines() = %v, want %v", got, want)
	}
	if strings.Contains(buf.String(), "not fully collected") {
		t.Errorf("WaitDone() on a completed drain logged an abandonment warning; output = %q", buf.String())
	}
}

// TestStderrCollector_AbandonAfterWaitDoneTimeout drives WaitDone and Abandon
// over an io.Pipe whose write end the test holds open, so the drain goroutine
// is still running (blocked in Read) while the abandoned readers run
// concurrently with it. The race detector is the evidence that the abandoned
// path reads none of the drain goroutine's state.
func TestStderrCollector_AbandonAfterWaitDoneTimeout(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	c := NewStderrCollector(pr, logger)

	const bound = 50 * time.Millisecond

	if c.WaitDone(bound) {
		t.Fatal("WaitDone() = true over a pipe whose write end the test holds open, want false")
	}
	if buf.Len() != 0 {
		t.Errorf("WaitDone() over an unfinished drain logged output; want none; got %q", buf.String())
	}

	c.Abandon(bound)
	afterFirst := buf.String()
	if !strings.Contains(afterFirst, "not fully collected") {
		t.Errorf("Abandon() did not log the abandonment warning; output = %q", afterFirst)
	}
	if !strings.Contains(afterFirst, "drain_bound") {
		t.Errorf("Abandon() warning missing the drain_bound attribute; output = %q", afterFirst)
	}

	c.Abandon(bound)
	if got := buf.String(); got != afterFirst {
		t.Errorf("second Abandon() call logged another record; output = %q", got)
	}

	select {
	case <-c.Done():
		t.Error("Done() closed after Abandon(); want it to stay open until the drain finishes")
	default:
	}

	type readerResult struct {
		lines   []string
		dropped int
	}
	resCh := make(chan readerResult, 1)
	go func() {
		resCh <- readerResult{lines: c.Lines(), dropped: c.Dropped()}
	}()

	var res readerResult
	select {
	case res = <-resCh:
	case <-time.After(1 * time.Second):
		t.Fatal("Lines()/Dropped() blocked more than 1 second after Abandon()")
	}
	if want := []string{AbandonedMarker}; !slices.Equal(res.lines, want) {
		t.Errorf("Lines() after Abandon() = %v, want %v", res.lines, want)
	}
	if res.dropped != 0 {
		t.Errorf("Dropped() after Abandon() = %d, want 0", res.dropped)
	}

	warnDone := make(chan struct{})
	go func() {
		EmitWarnLines(c.Lines(), logger)
		close(warnDone)
	}()
	select {
	case <-warnDone:
	case <-time.After(1 * time.Second):
		t.Fatal("EmitWarnLines(c.Lines(), ...) blocked more than 1 second after Abandon()")
	}
	if !strings.Contains(buf.String(), AbandonedMarker) {
		t.Errorf("EmitWarnLines(c.Lines(), ...) after Abandon() did not emit the marker; output = %q", buf.String())
	}
}

// TestStderrCollector_WaitDonePolling pins the poll semantics of a
// non-positive duration: it reports the current state immediately rather
// than waiting, whatever that state is.
func TestStderrCollector_WaitDonePolling(t *testing.T) {
	t.Parallel()

	t.Run("finished drain", func(t *testing.T) {
		t.Parallel()

		c := NewStderrCollector(strings.NewReader("done\n"), slog.Default())
		<-c.Done()

		for _, d := range []time.Duration{0, -1 * time.Millisecond} {
			if !c.WaitDone(d) {
				t.Errorf("WaitDone(%v) on a finished drain = false, want true", d)
			}
		}
	})

	t.Run("unfinished drain", func(t *testing.T) {
		t.Parallel()

		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		c := NewStderrCollector(pr, slog.Default())

		for _, d := range []time.Duration{0, -1 * time.Millisecond} {
			if c.WaitDone(d) {
				t.Errorf("WaitDone(%v) on an unfinished drain = true, want false", d)
			}
		}
	})
}

// TestStderrCollector_AbandonmentPriority pins the precedence in both
// directions: a drain that finishes always outranks abandonment,
// whichever happened first. It also pins that abandoning an
// already-finished drain has no effect.
func TestStderrCollector_AbandonmentPriority(t *testing.T) {
	t.Parallel()

	t.Run("lines call already blocked returns the marker", func(t *testing.T) {
		t.Parallel()

		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		c := NewStderrCollector(pr, slog.Default())

		resCh := make(chan []string, 1)
		go func() { resCh <- c.Lines() }()
		time.Sleep(50 * time.Millisecond)
		c.Abandon(50 * time.Millisecond)

		select {
		case got := <-resCh:
			if want := []string{AbandonedMarker}; !slices.Equal(got, want) {
				t.Errorf("Lines() blocked before Abandon() = %v, want %v", got, want)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("Lines() call already blocked when Abandon() ran did not unblock within 1 second")
		}
	})

	t.Run("drain finishing after abandonment wins on the next call", func(t *testing.T) {
		t.Parallel()

		pr, pw := io.Pipe()
		c := NewStderrCollector(pr, slog.Default())

		c.Abandon(50 * time.Millisecond)
		if got := c.Lines(); !slices.Equal(got, []string{AbandonedMarker}) {
			t.Fatalf("Lines() right after Abandon() = %v, want [%q]", got, AbandonedMarker)
		}

		if _, err := io.WriteString(pw, "real line\n"); err != nil {
			t.Fatalf("pw.Write(real line) = %v", err)
		}
		if err := pw.Close(); err != nil {
			t.Fatalf("pw.Close() = %v", err)
		}

		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("Done() did not close after the reader reached EOF")
		}

		want := []string{"real line"}
		if got := c.Lines(); !slices.Equal(got, want) {
			t.Errorf("Lines() after the drain finished = %v, want %v", got, want)
		}
	})

	t.Run("abandon on an already-finished drain is a no-op", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		c := NewStderrCollector(strings.NewReader("already collected\n"), logger)
		<-c.Done()

		c.Abandon(50 * time.Millisecond)

		if buf.Len() != 0 {
			t.Errorf("Abandon() on a finished drain logged output; want none; got %q", buf.String())
		}
		want := []string{"already collected"}
		if got := c.Lines(); !slices.Equal(got, want) {
			t.Errorf("Lines() after Abandon() on a finished drain = %v, want %v", got, want)
		}
	})
}

// TestStderrCollector_FinishAndCollect_HealthyDrain pins the plain path:
// a drain that finishes on its own returns the real lines with no
// abandonment marker, and does not wait anywhere near the grace bound.
func TestStderrCollector_FinishAndCollect_HealthyDrain(t *testing.T) {
	t.Parallel()

	c := NewStderrCollector(strings.NewReader("codex: ready\n"), slog.Default())

	start := time.Now()
	got := c.FinishAndCollect(DefaultDrainGrace)
	if elapsed := time.Since(start); elapsed >= DefaultDrainGrace {
		t.Errorf("FinishAndCollect(%v) on a finished drain took %v, want well under the bound", DefaultDrainGrace, elapsed)
	}

	want := []string{"codex: ready"}
	if !slices.Equal(got, want) {
		t.Errorf("FinishAndCollect(%v) = %v, want %v", DefaultDrainGrace, got, want)
	}
}

// TestStderrCollector_FinishAndCollect_PreservesLinesOnAbandonment pins
// the operator-facing point of FinishAndCollect: a drain that never
// finishes still hands back whatever it had already collected, with
// [AbandonedMarker] appended after it rather than in place of it.
func TestStderrCollector_FinishAndCollect_PreservesLinesOnAbandonment(t *testing.T) {
	t.Parallel()

	const grace = 100 * time.Millisecond

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	c := NewStderrCollector(pr, slog.Default())

	if _, err := io.WriteString(pw, "codex: fatal provider error\n"); err != nil {
		t.Fatalf("pw.Write(line) = %v", err)
	}

	got := c.FinishAndCollect(grace)

	want := []string{"codex: fatal provider error", AbandonedMarker}
	if !slices.Equal(got, want) {
		t.Errorf("FinishAndCollect(%v) on an abandoned drain = %v, want %v (collected output must survive, marker appended after it)", grace, got, want)
	}
}

// TestStderrCollector_FinishAndCollect_SecondCallDoesNotWaitAgain pins
// that FinishAndCollect pays its bounded wait once per collector
// lifetime: a call that arrives after an earlier call already resolved
// the wait (by abandoning a drain that never finishes) returns near
// instantly rather than paying a fresh grace period of its own.
func TestStderrCollector_FinishAndCollect_SecondCallDoesNotWaitAgain(t *testing.T) {
	t.Parallel()

	const grace = 200 * time.Millisecond

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	c := NewStderrCollector(pr, slog.Default())

	c.FinishAndCollect(grace)

	start := time.Now()
	c.FinishAndCollect(grace)
	if elapsed := time.Since(start); elapsed > grace/2 {
		t.Errorf("FinishAndCollect(%v) called again after the wait already resolved took %v, want near-instant: a second bounded wait was paid", grace, elapsed)
	}
}

// TestStderrCollector_FinishAndCollect_LateCallerSharesRemainder pins
// the shared-wait semantics for a caller that arrives while an earlier
// call is still in flight: it pays only the remainder of the wait
// already under way, not a fresh grace period timed from its own
// arrival. A caller arriving at grace/2 whose own elapsed time comes
// out close to a full grace, rather than close to grace/2, is proof the
// wait was paid twice.
func TestStderrCollector_FinishAndCollect_LateCallerSharesRemainder(t *testing.T) {
	t.Parallel()

	const grace = 400 * time.Millisecond

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	c := NewStderrCollector(pr, slog.Default())

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		c.FinishAndCollect(grace)
	}()

	time.Sleep(grace / 2)

	start := time.Now()
	c.FinishAndCollect(grace)
	elapsed := time.Since(start)

	select {
	case <-firstDone:
	case <-time.After(grace):
		t.Fatal("the first FinishAndCollect call had not returned within another full grace period")
	}

	if elapsed >= grace*3/4 {
		t.Errorf("FinishAndCollect(%v) arriving at grace/2 took %v for its own elapsed time, want well under a full grace (the wait is shared, not paid twice)", grace, elapsed)
	}
}
