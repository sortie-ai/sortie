package procutil

import (
	"bytes"
	"log/slog"
	"sync"

	"github.com/sortie-ai/sortie/internal/redact"
)

// TailBuffer retains the last max bytes written to it, masking every
// registered secret value before applying the retention cut. It
// implements [io.Writer] for use as a bounded capture sink, such as a
// workspace hook's combined-output stream or the kiro credential
// guard's standard error, and is safe for concurrent use.
type TailBuffer struct {
	mu        sync.Mutex
	masker    *redact.Writer
	buf       bytes.Buffer
	max       int
	truncated bool
}

// NewTailBuffer constructs a [TailBuffer] retaining at most max bytes
// of the masked stream. It panics when max is not positive.
func NewTailBuffer(max int) *TailBuffer {
	if max <= 0 {
		panic("procutil: NewTailBuffer requires a positive max")
	}
	b := &TailBuffer{max: max}
	b.masker = redact.NewWriter(&tailBufferSink{b: b})
	return b
}

// tailBufferSink adapts TailBuffer's retention cut to [redact.Writer]'s
// destination, so every byte the writer forwards has already been
// masked before the cut sees it.
type tailBufferSink struct {
	b *TailBuffer
}

func (s *tailBufferSink) Write(p []byte) (int, error) {
	s.b.writeRetainedLocked(p)
	return len(p), nil
}

// Write passes p through the buffer's masker before applying the
// retention cut, so the retained bytes are the last max bytes of the
// masked stream. It always returns len(p), nil.
func (b *TailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, err := b.masker.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// writeRetainedLocked applies the retention cut to already-masked
// bytes. Callers must hold b.mu (through the masker's own Write, which
// this buffer's sink invokes synchronously).
func (b *TailBuffer) writeRetainedLocked(p []byte) {
	if len(p) > b.max {
		b.buf.Reset()
		b.buf.Write(p[len(p)-b.max:]) //nolint:errcheck // bytes.Buffer.Write never returns an error
		b.truncated = true
		return
	}

	b.buf.Write(p) //nolint:errcheck // bytes.Buffer.Write never returns an error
	if overflow := b.buf.Len() - b.max; overflow > 0 {
		b.buf.Next(overflow)
		b.truncated = true
	}
}

// Bytes flushes the buffer's masker and returns a copy of the retained,
// masked bytes.
func (b *TailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.flushLocked()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out
}

// Truncated flushes the buffer's masker and reports whether a discard
// has happened.
func (b *TailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.flushLocked()
	return b.truncated
}

// flushLocked flushes the masker into the retained buffer. Callers
// must hold b.mu.
func (b *TailBuffer) flushLocked() {
	_ = b.masker.Flush() // the sink never returns an error
}

// Collector flushes the buffer's masker and wraps the retained bytes
// in a [StderrCollector] whose drain has already reached end of file,
// under the default line and byte caps. Call it only after the capture
// writing the buffer has returned. A nil logger resolves to
// [slog.Default].
func (b *TailBuffer) Collector(logger *slog.Logger) *StderrCollector {
	retained := b.Bytes()
	return NewStderrCollector(bytes.NewReader(retained), logger)
}
