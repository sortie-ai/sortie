package redact

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestWriter_DoesNotPanicWithEmptyRegistry runs sequentially, before any
// t.Parallel test in this package pauses to join the parallel batch, so it
// is the one place this process's secret registry is still guaranteed
// empty (every other sequential test in this package registers only a
// piece under MinValueBytes, which addPieces drops without adding it).
// forwardLocked computes its safe-to-forward cut from the longest
// registered value's length; with zero registered values that length is 0,
// and cut = len(pending) - (0 - 1) overshoots len(pending), which the
// subsequent slice and make() calls do not expect.
//
// It also registers a throwaway value once recover has run, so every
// later, parallel test in this package writes against a non-empty
// registry regardless of scheduling order.
func TestWriter_DoesNotPanicWithEmptyRegistry(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Write() panicked with no value registered in the process: %v", r)
		}
		Add("test.Writer empty-registry primer", "writer-empty-registry-primer-value")
	}()

	var dst bytes.Buffer
	w := NewWriter(&dst)
	if _, err := w.Write([]byte("0123456789ABCDEFGHIJ")); err != nil {
		t.Errorf("Write() error = %v, want nil", err)
	}
}

func TestWriter_WriteAlwaysReportsFullLength(t *testing.T) {
	t.Parallel()

	var dst bytes.Buffer
	w := NewWriter(&dst)

	p := []byte("some arbitrary bytes to write through the masker")
	n, err := w.Write(p)
	if err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if n != len(p) {
		t.Errorf("Write() = %d, want %d", n, len(p))
	}
}

func TestWriter_MatchesMaskForAnySplitOfOverlappingValues(t *testing.T) {
	t.Parallel()

	unique := randomSecret(t)
	shared := "shared" + unique
	left := "AAAAAAAA" + shared
	right := shared + "BBBBBBBB"
	Add("test.Writer overlap "+unique, left)
	Add("test.Writer overlap "+unique, right)

	full := "prefix-" + left + "BBBBBBBB-suffix"
	want := Mask(full)

	for split := 0; split <= len(full); split++ {
		t.Run(fmt.Sprintf("split at byte %d", split), func(t *testing.T) {
			t.Parallel()

			var dst bytes.Buffer
			w := NewWriter(&dst)
			if _, err := w.Write([]byte(full[:split])); err != nil {
				t.Fatalf("Write(first half) error = %v", err)
			}
			if _, err := w.Write([]byte(full[split:])); err != nil {
				t.Fatalf("Write(second half) error = %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush() error = %v", err)
			}
			if got := dst.String(); got != want {
				t.Errorf("split at %d: dst = %q, want %q (Mask of the whole input)", split, got, want)
			}
		})
	}
}

func TestWriter_MatchesMaskForByteAtATimeWrites(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.Writer byte-at-a-time", value)

	full := "before-" + value + "-after"
	want := Mask(full)

	var dst bytes.Buffer
	w := NewWriter(&dst)
	for i := range len(full) {
		if _, err := w.Write([]byte{full[i]}); err != nil {
			t.Fatalf("Write(byte %d) error = %v", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := dst.String(); got != want {
		t.Errorf("byte-at-a-time dst = %q, want %q", got, want)
	}
}

func TestWriter_ForwardsNonMatchingTextEagerly(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.Writer eager forward", value)

	var dst bytes.Buffer
	w := NewWriter(&dst)
	if _, err := w.Write([]byte(strings.Repeat("no secrets here ", 20))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if dst.Len() == 0 {
		t.Error("Write() of clearly non-matching text forwarded nothing before Flush, want eager forwarding")
	}
}

func TestWriter_HoldsBackAPartialMatchUntilItResolves(t *testing.T) {
	t.Parallel()

	value := randomSecret(t) // well over MinValueBytes
	Add("test.Writer partial match", value)

	half := len(value) / 2
	var dst bytes.Buffer
	w := NewWriter(&dst)
	if _, err := w.Write([]byte("start-" + value[:half])); err != nil {
		t.Fatalf("Write(first half) error = %v", err)
	}
	if strings.Contains(dst.String(), value[:half]) {
		t.Fatalf("Write() forwarded the unresolved prefix %q of a value that might still complete a match", value[:half])
	}

	if _, err := w.Write([]byte(value[half:] + "-end")); err != nil {
		t.Fatalf("Write(second half) error = %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	got := dst.String()
	if strings.Contains(got, value) {
		t.Fatalf("dst = %q, still contains the registered value split across two writes", got)
	}
	if !strings.Contains(got, Marker) {
		t.Errorf("dst = %q, want it to contain %q", got, Marker)
	}
}

func TestWriter_FlushOnEmptyPendingIsNoop(t *testing.T) {
	t.Parallel()

	var dst bytes.Buffer
	w := NewWriter(&dst)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() on an empty writer error = %v, want nil", err)
	}
	if dst.Len() != 0 {
		t.Errorf("Flush() on an empty writer wrote %d bytes, want 0", dst.Len())
	}
}

func TestWriter_ConcurrentWritesPreserveTotalByteCount(t *testing.T) {
	t.Parallel()

	var dst bytes.Buffer
	w := NewWriter(&dst)

	const goroutines = 8
	const chunkLen = 32
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chunk := strings.Repeat(fmt.Sprintf("%02x", i), chunkLen/2)
			if _, err := w.Write([]byte(chunk)); err != nil {
				t.Errorf("Write() from goroutine %d error = %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	if got, want := dst.Len(), goroutines*chunkLen; got != want {
		t.Errorf("concurrent Write() total forwarded = %d bytes, want %d", got, want)
	}
}
