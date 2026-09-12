package procutil

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStdoutReader_StreamDeliversEachLineOnceInOrder asserts that every
// scanned line is delivered exactly once, in order, and that the
// delivered slice survives whatever the scanner does with its own
// internal buffer afterward (each delivery is an independent copy).
func TestStdoutReader_StreamDeliversEachLineOnceInOrder(t *testing.T) {
	t.Parallel()

	want := []string{"first", "second", "third"}
	r := NewStdoutReader(strings.NewReader(strings.Join(want, "\n")+"\n"), nil)

	var got []string
	for line := range r.Stream() {
		got = append(got, string(line))
	}

	if len(got) != len(want) {
		t.Fatalf("Stream() delivered %d lines, want %d: got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("Stream() line %d = %q, want %q", i, got[i], w)
		}
	}

	select {
	case <-r.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("Done() did not close after Stream() closed")
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err() = %v, want nil (clean end of file)", err)
	}
}

// TestStdoutReader_AbandonOneShot asserts that Abandon is a one-shot
// latch: a second call has no further effect, and a call after the scan
// has already finished emits no WARN record.
func TestStdoutReader_AbandonOneShot(t *testing.T) {
	t.Parallel()

	t.Run("second call logs nothing further", func(t *testing.T) {
		t.Parallel()

		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.Close() }) //nolint:errcheck // best-effort

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		r := NewStdoutReader(pr, logger)

		const bound = 50 * time.Millisecond
		r.Abandon(bound)
		first := buf.String()
		if !strings.Contains(first, "agent stdout was not fully collected before the turn ended") {
			t.Fatalf("Abandon() did not log the abandonment warning; output = %q", first)
		}

		r.Abandon(bound)
		if got := buf.String(); got != first {
			t.Errorf("second Abandon() call logged another record; output = %q", got)
		}
	})

	t.Run("call after the scan finished emits no WARN", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		r := NewStdoutReader(strings.NewReader("line one\n"), logger)

		for range r.Stream() {
		}
		select {
		case <-r.Done():
		case <-time.After(1 * time.Second):
			t.Fatal("Done() did not close after Stream() closed")
		}

		r.Abandon(50 * time.Millisecond)
		if got := buf.String(); got != "" {
			t.Errorf("Abandon() after the scan finished logged %q, want no output", got)
		}
		if err := r.Err(); err != nil {
			t.Errorf("Err() = %v, want nil (Abandon after completion must not overwrite the real outcome)", err)
		}
	})
}

// TestStdoutReader_AbandonLogsMessageAndBound asserts the WARN record's
// fixed message text and its drain_bound attribute.
func TestStdoutReader_AbandonLogsMessageAndBound(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() }) //nolint:errcheck // best-effort

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := NewStdoutReader(pr, logger)

	r.Abandon(250 * time.Millisecond)

	output := buf.String()
	if !strings.Contains(output, "msg=\"agent stdout was not fully collected before the turn ended\"") {
		t.Errorf("Abandon() did not log the fixed message text: %q", output)
	}
	if !strings.Contains(output, "drain_bound=250ms") {
		t.Errorf("Abandon() did not log drain_bound=250ms: %q", output)
	}
}

// TestStdoutReader_Err asserts Err's three outcomes: nil on a clean end
// of file, the scanner's own error on a read failure (including a line
// exceeding the maximum token size), and ErrStdoutAbandoned when Abandon
// fired before the scan otherwise ended.
func TestStdoutReader_Err(t *testing.T) {
	t.Parallel()

	t.Run("nil on EOF", func(t *testing.T) {
		t.Parallel()
		r := NewStdoutReader(strings.NewReader("ok\n"), nil)
		for range r.Stream() {
		}
		if err := r.Err(); err != nil {
			t.Errorf("Err() = %v, want nil", err)
		}
	})

	t.Run("scanner error on a read failure", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("boom")
		r := NewStdoutReader(&failingReader{err: wantErr}, nil)
		for range r.Stream() {
		}
		if err := r.Err(); !errors.Is(err, wantErr) {
			t.Errorf("Err() = %v, want %v", err, wantErr)
		}
	})

	t.Run("bufio.ErrTooLong on an oversized line", func(t *testing.T) {
		t.Parallel()
		oversized := strings.Repeat("x", DefaultScannerMaxSize+1) + "\n"
		r := NewStdoutReader(strings.NewReader(oversized), nil)
		for range r.Stream() {
		}
		if err := r.Err(); !errors.Is(err, bufio.ErrTooLong) {
			t.Errorf("Err() = %v, want %v", err, bufio.ErrTooLong)
		}
	})

	t.Run("ErrStdoutAbandoned when Abandon fires first", func(t *testing.T) {
		t.Parallel()
		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.Close() }) //nolint:errcheck // best-effort

		r := NewStdoutReader(pr, slog.New(slog.DiscardHandler))

		// Abandon only takes effect once the scan goroutine is parked
		// delivering a scanned line: a goroutine still blocked inside its
		// first Read, as one would be over a pipe nothing has ever
		// written to, is released only by closing the underlying reader,
		// not by Abandon alone. Writing one line first reproduces the
		// state Abandon is actually invoked against in production: a
		// descendant still holding the handle open after everything the
		// direct child wrote has already been scanned.
		if _, err := pw.Write([]byte("parked line\n")); err != nil {
			t.Fatalf("pw.Write() = %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		r.Abandon(50 * time.Millisecond)

		select {
		case <-r.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("Done() did not close after Abandon()")
		}
		if err := r.Err(); !errors.Is(err, ErrStdoutAbandoned) {
			t.Errorf("Err() = %v, want %v", err, ErrStdoutAbandoned)
		}
	})
}

// TestStdoutReader_AbandonReleasesAParkedSend asserts that Abandon
// unparks a scan goroutine parked on a send to an unread Stream, rather
// than requiring the consumer to keep draining.
func TestStdoutReader_AbandonReleasesAParkedSend(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() }) //nolint:errcheck // best-effort

	r := NewStdoutReader(pr, slog.New(slog.DiscardHandler))

	if _, err := pw.Write([]byte("parked line\n")); err != nil {
		t.Fatalf("pw.Write() = %v", err)
	}

	// Give the scan goroutine a chance to reach the parked send before
	// abandoning; a race here only makes the test slower, never wrong,
	// because Abandon's effect is what closes Done regardless of whether
	// the goroutine had already reached the send.
	time.Sleep(50 * time.Millisecond)

	r.Abandon(50 * time.Millisecond)

	select {
	case <-r.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after Abandon() released a goroutine parked on a send")
	}
}

// failingReader always returns err from Read.
type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// parkedReader blocks its first Read until released, and reports when
// the scan goroutine has actually entered it. Without that signal a
// test racing Abandon against the scan can abandon a reader that is not
// yet parked, and pass while proving nothing.
type parkedReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *parkedReader) Read([]byte) (int, error) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return 0, io.EOF
}

func newParkedReader() *parkedReader {
	return &parkedReader{entered: make(chan struct{}), release: make(chan struct{})}
}

// TestStdoutReader_AbandonWhileParkedInRead asserts that a reader
// abandoned while its scan is parked in a read reports the abandonment
// rather than whatever the eventual release produces. This is the
// ordinary shape of abandonment, not an edge case: a consumer gives up
// precisely because the scan is parked on a handle a descendant holds,
// and the close that finally releases it is the consumer's own.
func TestStdoutReader_AbandonWhileParkedInRead(t *testing.T) {
	t.Parallel()

	src := newParkedReader()
	r := NewStdoutReader(src, slog.New(slog.DiscardHandler))

	<-src.entered
	r.Abandon(time.Second)

	if err := r.Err(); !errors.Is(err, ErrStdoutAbandoned) {
		t.Fatalf("Err() while the scan is still parked = %v, want ErrStdoutAbandoned", err)
	}

	close(src.release)

	select {
	case <-r.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after the read was released")
	}

	if err := r.Err(); !errors.Is(err, ErrStdoutAbandoned) {
		t.Errorf("Err() after the scan ended = %v, want ErrStdoutAbandoned; "+
			"the clean end of the released read must not replace the abandonment", err)
	}
}

// TestStdoutReader_AbandonAfterScanEndedIsNoOp asserts the reverse
// interleaving: a scan that has already finished keeps its own outcome,
// and Abandon neither rewrites it nor warns about a reader nobody gave
// up on.
func TestStdoutReader_AbandonAfterScanEndedIsNoOp(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	r := NewStdoutReader(strings.NewReader("only\n"),
		slog.New(slog.NewTextHandler(&logged, nil)))

	for range r.Stream() {
	}
	<-r.Done()

	r.Abandon(time.Second)

	if err := r.Err(); err != nil {
		t.Errorf("Err() = %v, want nil; Abandon after completion must not reclassify a finished scan", err)
	}
	if logged.Len() != 0 {
		t.Errorf("Abandon after completion logged %q, want no record", logged.String())
	}
}

// TestStdoutReader_ErrIsRaceFreeBeforeDone asserts Err may be called
// from another goroutine while the scan is still running, and keeps
// calling it across the moment the scan publishes its outcome. The race
// detector is the assertion, and the parked source is what guarantees
// the two actually overlap.
func TestStdoutReader_ErrIsRaceFreeBeforeDone(t *testing.T) {
	t.Parallel()

	src := newParkedReader()
	r := NewStdoutReader(src, slog.New(slog.DiscardHandler))

	<-src.entered

	stop := make(chan struct{})
	polling := make(chan struct{})
	go func() {
		defer close(polling)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.Err()
		}
	}()

	// Released while the poller is still running, so the scan's write of
	// its outcome overlaps the reads.
	close(src.release)
	<-r.Done()
	close(stop)
	<-polling

	if err := r.Err(); err != nil {
		t.Errorf("Err() = %v, want nil (clean end of the released read)", err)
	}
}
