//go:build unix

package probe

import (
	"bytes"
	"errors"
	"strings"
)

// errNativeLaunchFailed marks a native surface launch that never
// started, distinguishable from a bounded exit or timeout.
var errNativeLaunchFailed = errors.New("native probe failed to launch")

// errNativeBoundExceeded marks a probe that exceeded its bounded wait,
// distinguishable from a non-zero-exit run failure via errors.Is.
var errNativeBoundExceeded = errors.New("the native probe exceeded its bounded wait")

// lineBoundedWriter accumulates newline-delimited lines up to limit
// bytes each, dropping and counting any single line that exceeds it.
type lineBoundedWriter struct {
	limit      int
	pending    []byte
	built      strings.Builder
	dropped    int
	discarding bool
	// peak is the high-water mark of the pending buffer. The bound is a
	// claim about memory held mid-write, which no assertion made after
	// Write returns can observe, so the writer records it.
	peak int
}

func (w *lineBoundedWriter) buffer(b []byte) {
	w.pending = append(w.pending, b...)
	w.peak = max(w.peak, len(w.pending))
}

// Write implements io.Writer. A line that cannot fit is counted and
// abandoned before any byte of it is buffered, so the buffer never
// exceeds limit whatever a single write carries.
func (w *lineBoundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if w.discarding {
			if idx < 0 {
				return n, nil
			}
			w.discarding = false
			p = p[idx+1:]
			continue
		}
		if idx < 0 {
			if len(w.pending)+len(p) > w.limit {
				w.dropped++
				w.discarding = true
				w.pending = nil
				return n, nil
			}
			w.buffer(p)
			return n, nil
		}
		if len(w.pending)+idx+1 > w.limit {
			w.dropped++
		} else {
			w.buffer(p[:idx+1])
			w.emit(w.pending)
		}
		w.pending = nil
		p = p[idx+1:]
	}
	return n, nil
}

func (w *lineBoundedWriter) emit(line []byte) {
	if len(line) > w.limit {
		w.dropped++
		return
	}
	w.built.Write(line)
}

// Flush emits any trailing partial line. Call it once no further data
// will arrive.
func (w *lineBoundedWriter) Flush() {
	if len(w.pending) == 0 {
		return
	}
	w.emit(w.pending)
	w.pending = nil
}

func (w *lineBoundedWriter) String() string {
	return w.built.String()
}
