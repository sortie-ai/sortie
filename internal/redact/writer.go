package redact

import (
	"io"
	"slices"
	"strings"
	"sync"
)

// Writer masks every registered secret value in a byte stream, forwarding
// output to dst as soon as no later byte can change whether it is part of
// a match. After [Writer.Flush], what dst received equals [Mask] of
// everything written, for any split of that input into [Writer.Write]
// calls, provided no value was registered in between. Safe for
// concurrent use.
type Writer struct {
	mu      sync.Mutex
	dst     io.Writer
	pending []byte
}

// NewWriter wraps dst so every byte written through the result is masked
// before dst sees it.
func NewWriter(dst io.Writer) *Writer {
	return &Writer{dst: dst}
}

// Write buffers p and forwards to dst the prefix of the accumulated
// stream that no later byte can still extend into a match, always
// returning len(p), nil unless dst.Write fails.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending = append(w.pending, p...)
	if err := w.forwardLocked(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush forwards everything still held in pending to dst.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.forwardLocked(true)
}

// forwardLocked writes the safely-decided prefix of w.pending to dst,
// masking it, and retains the rest. flush forces the whole buffer out.
// Callers must hold w.mu.
func (w *Writer) forwardLocked(flush bool) error {
	if len(w.pending) == 0 {
		return nil
	}

	values := loadRegistered()
	maxLen := longestLen(values)

	cut := len(w.pending)
	if !flush {
		hold := max(0, maxLen-1)
		cut = max(0, len(w.pending)-hold)
		for _, sp := range findSpans(w.pending, values) {
			if sp.start < cut && sp.end > cut {
				cut = sp.start
			}
		}
	}
	if cut == 0 {
		return nil
	}

	out := maskBytes(w.pending[:cut], findSpans(w.pending[:cut], values))
	if _, err := w.dst.Write(out); err != nil {
		return err
	}

	remaining := make([]byte, len(w.pending)-cut)
	copy(remaining, w.pending[cut:])
	w.pending = remaining
	return nil
}

func longestLen(values []string) int {
	longest := 0
	for _, v := range values {
		longest = max(longest, len(v))
	}
	return longest
}

type matchSpan struct{ start, end int }

// findSpans returns the merged occurrence spans of every value in
// values within data, sorted and combined exactly as [Mask] combines
// them.
func findSpans(data []byte, values []string) []matchSpan {
	var spans []matchSpan
	s := string(data)
	for _, v := range values {
		if v == "" {
			continue
		}
		for start := 0; ; {
			idx := indexString(s, v, start)
			if idx < 0 {
				break
			}
			spans = append(spans, matchSpan{idx, idx + len(v)})
			start = idx + 1
		}
	}
	if len(spans) == 0 {
		return nil
	}

	slices.SortFunc(spans, func(a, b matchSpan) int { return a.start - b.start })
	merged := spans[:1]
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp.start <= last.end {
			if sp.end > last.end {
				last.end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

func indexString(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	idx := strings.Index(s[from:], sub)
	if idx < 0 {
		return -1
	}
	return from + idx
}

func maskBytes(data []byte, spans []matchSpan) []byte {
	if len(spans) == 0 {
		out := make([]byte, len(data))
		copy(out, data)
		return out
	}

	out := make([]byte, 0, len(data))
	last := 0
	for _, sp := range spans {
		out = append(out, data[last:sp.start]...)
		out = append(out, Marker...)
		last = sp.end
	}
	out = append(out, data[last:]...)
	return out
}
