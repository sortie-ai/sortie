package procutil

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ErrStdoutAbandoned reports a scan the adapter gave up on.
var ErrStdoutAbandoned = errors.New("procutil: standard output reader abandoned")

// StdoutReader scans a subprocess's standard output line by line and
// delivers each line to one consumer. A consumer that gives up on it
// calls Abandon, after which the reader delivers nothing further. Its
// method set mirrors StderrCollector's.
type StdoutReader struct {
	stream chan []byte
	done   chan struct{}
	logger *slog.Logger

	// mu guards err, which the scan goroutine writes once when its scan
	// ends and Err reads. A consumer may call Err while the scan is
	// still parked in a read, so the two cannot be ordered by the
	// consumer alone.
	mu  sync.Mutex
	err error

	abandonOnce sync.Once
	abandoned   chan struct{}
}

// NewStdoutReader starts one goroutine that scans r line by line with a
// 10 MB maximum token size and a 64 KB initial buffer, sending a copy of
// each line on the channel Stream returns. logger receives the record
// Abandon emits; a nil logger resolves to slog.Default.
func NewStdoutReader(r io.Reader, logger *slog.Logger) *StdoutReader {
	if logger == nil {
		logger = slog.Default()
	}
	sr := &StdoutReader{
		stream:    make(chan []byte),
		done:      make(chan struct{}),
		logger:    logger,
		abandoned: make(chan struct{}),
	}
	go sr.scan(r)
	return sr
}

func (r *StdoutReader) scan(src io.Reader) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), DefaultScannerMaxSize)

	abandoned := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case r.stream <- line:
		case <-r.abandoned:
			abandoned = true
		}
		if abandoned {
			break
		}
	}

	// Read the latch rather than the local flag: Abandon can close it
	// while this goroutine is parked in a read, and the read then ends
	// on the pipe's close with a close error the consumer must not see
	// as a scan failure.
	r.mu.Lock()
	select {
	case <-r.abandoned:
		r.err = ErrStdoutAbandoned
	default:
		r.err = scanner.Err()
	}
	r.mu.Unlock()

	close(r.stream)
	close(r.done)
}

// Stream returns the unbuffered channel the reader delivers scanned
// lines on. Each received slice is owned by the consumer. The channel
// closes when the scan goroutine returns, and never because of Abandon
// alone.
func (r *StdoutReader) Stream() <-chan []byte {
	return r.stream
}

// Done closes once the scan goroutine has finished reading its source
// to EOF, a read error, or an observed abandonment.
func (r *StdoutReader) Done() <-chan struct{} {
	return r.done
}

// WaitDone waits up to d for the scan goroutine to finish and reports
// whether it finished. It has no side effect: a false return leaves the
// reader exactly as it was.
//
// A non-positive d polls once rather than waiting unboundedly.
func (r *StdoutReader) WaitDone(d time.Duration) bool {
	select {
	case <-r.done:
		return true
	default:
	}
	if d <= 0 {
		return false
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-r.done:
		return true
	case <-timer.C:
		select {
		case <-r.done:
			return true
		default:
			return false
		}
	}
}

// Abandon gives up on a scan a bounded wait did not finish, so the
// reader's goroutine stops delivering lines. bound is the wait that
// elapsed, reported with the warning Abandon logs. It is a one-shot
// latch and does nothing when the scan has already returned.
func (r *StdoutReader) Abandon(bound time.Duration) {
	r.abandonOnce.Do(func() {
		select {
		case <-r.done:
			return
		default:
		}
		r.logger.Warn("agent stdout was not fully collected before the turn ended",
			slog.Duration("drain_bound", bound))
		close(r.abandoned)
	})
}

// Err reports why the scan ended: nil on end of file or while the scan
// is still running, the scanner's error on a read failure, and
// ErrStdoutAbandoned once the reader has been abandoned.
//
// Abandonment answers immediately and does not wait for the scan
// goroutine. A consumer abandons a reader precisely when that goroutine
// is parked in a read nothing but the pipe's close will release, so
// waiting for it here would block the caller that just gave up on it.
func (r *StdoutReader) Err() error {
	select {
	case <-r.abandoned:
		return ErrStdoutAbandoned
	default:
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
