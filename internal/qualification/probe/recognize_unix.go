//go:build unix

package probe

import (
	"bytes"
	"errors"
	"strings"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// errNativeLaunchFailed marks a native surface launch that never
// started, distinguishable from a bounded exit or timeout.
var errNativeLaunchFailed = errors.New("native probe failed to launch")

// errNativeBoundExceeded marks a probe that exceeded its bounded wait,
// distinguishable from a non-zero-exit run failure via errors.Is.
var errNativeBoundExceeded = errors.New("the native probe exceeded its bounded wait")

// lineBoundedWriter accumulates newline-delimited lines up to limit
// bytes each, dropping any single line that exceeds it instead of
// retaining it, and counting the lines it dropped. A stream-json
// treatment run echoes its filler payload as one line that can reach
// several megabytes; without this bound that echo would be retained
// for the whole classification.
type lineBoundedWriter struct {
	limit      int
	pending    []byte
	built      strings.Builder
	dropped    int
	discarding bool
	// peak is the high-water mark of the pending buffer. The bound is
	// a claim about memory held mid-write, which no assertion made
	// after Write returns can observe, so the writer records it.
	peak int
}

// buffer appends b to the pending line and records the high-water mark.
func (w *lineBoundedWriter) buffer(b []byte) {
	w.pending = append(w.pending, b...)
	w.peak = max(w.peak, len(w.pending))
}

// Write implements io.Writer, emitting every complete line p now
// closes. A line is counted and abandoned before any byte of it is
// buffered once it cannot fit, so the buffer never exceeds limit
// whatever a single write carries.
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

// emit retains line if it is within limit, and otherwise drops it and
// counts the drop.
func (w *lineBoundedWriter) emit(line []byte) {
	if len(line) > w.limit {
		w.dropped++
		return
	}
	w.built.Write(line)
}

// Flush emits any trailing partial line that never received a
// newline. Call it once the writer will receive no further data.
func (w *lineBoundedWriter) Flush() {
	if len(w.pending) == 0 {
		return
	}
	w.emit(w.pending)
	w.pending = nil
}

// String returns the writer's retained content.
func (w *lineBoundedWriter) String() string {
	return w.built.String()
}

// nativeTerminal recognizes one native surface's terminal outcome from
// its output, dispatching generically off the profile's own
// [qualification.Recognizer] for that surface in place of a
// per-runtime switch. It returns the recognized terminal, whether the
// probe suffered transport loss (a bounded exit or timeout with no
// recognized terminal member), and whether a terminal was recognized
// at all. A launch failure carries no terminal signal and is not
// transport loss: the binary never ran, so nothing about its protocol
// behavior was observed.
func nativeTerminal(profile qualification.RuntimeProfile, surface qualification.Surface, output string, launchErr error) (terminal qualification.Terminal, transportLoss bool, found bool) {
	if launchErr != nil {
		if errors.Is(launchErr, errNativeLaunchFailed) {
			return qualification.Terminal{}, false, false
		}
		return qualification.Terminal{}, true, true
	}
	recognizer, ok := profile.Recognizers[surface]
	if !ok {
		return qualification.Terminal{}, false, false
	}
	terminal, found = recognizer.Terminal(output)
	return terminal, false, found
}
