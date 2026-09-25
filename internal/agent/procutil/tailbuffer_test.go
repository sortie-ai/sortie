package procutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/redact"
)

// randomTailBufferSecret returns a fresh registered-secret value unique to
// this test run, so registrations one test makes can never be observed by
// another sharing the same process-wide redact registry.
func randomTailBufferSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "tailbuffer-secret-" + hex.EncodeToString(buf)
}

func TestNewTailBuffer_PanicsOnNonPositiveMax(t *testing.T) {
	t.Parallel()

	for _, max := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewTailBuffer(%d) did not panic, want a panic", max)
				}
			}()
			NewTailBuffer(max)
		}()
	}
}

// TestTailBuffer_WriteDoesNotPanicWithEmptyRegistry runs sequentially,
// before any t.Parallel test in this package pauses to join the parallel
// batch, so it is the one place this process's secret registry is still
// guaranteed empty. See the identically-named invariant documented on
// redact.TestWriter_DoesNotPanicWithEmptyRegistry: redact.Writer computes
// its safe-to-forward cut from the longest registered value's length, and
// an empty registry (length 0) overshoots the pending buffer's bounds.
//
// It also registers a throwaway value once recover has run, so every
// later, parallel test in this package writes against a non-empty
// registry regardless of scheduling order.
func TestTailBuffer_WriteDoesNotPanicWithEmptyRegistry(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Write() panicked with no value registered in the process: %v", r)
		}
		redact.Add("test.TailBuffer empty-registry primer", "tailbuffer-empty-registry-primer-value")
	}()

	b := NewTailBuffer(10)
	if _, err := b.Write([]byte("0123456789ABCDEFGHIJ")); err != nil {
		t.Errorf("Write() error = %v, want nil", err)
	}
}

func TestTailBuffer_WriteAlwaysReportsFullLength(t *testing.T) {
	t.Parallel()

	b := NewTailBuffer(64)
	n, err := b.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if n != len("hello world") {
		t.Errorf("Write() = %d, want %d", n, len("hello world"))
	}
}

func TestTailBuffer_RetainsAtMostMaxBytesFromTheEnd(t *testing.T) {
	t.Parallel()

	b := NewTailBuffer(10)
	if _, err := b.Write([]byte("0123456789ABCDEFGHIJ")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got, want := string(b.Bytes()), "ABCDEFGHIJ"; got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
	if !b.Truncated() {
		t.Error("Truncated() = false, want true after writing more than max bytes")
	}
}

func TestTailBuffer_UnderMaxIsNotTruncated(t *testing.T) {
	t.Parallel()

	b := NewTailBuffer(64)
	if _, err := b.Write([]byte("short output")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if b.Truncated() {
		t.Error("Truncated() = true for output under max, want false")
	}
	if got, want := string(b.Bytes()), "short output"; got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
}

func TestTailBuffer_RetentionCutAppliesToTheMaskedStream(t *testing.T) {
	t.Parallel()

	value := randomTailBufferSecret(t)
	redact.Add("test.TailBuffer retention", value)

	b := NewTailBuffer(len(redact.Marker) + 6) // room for "before-" trimmed to "e-" + marker
	if _, err := b.Write([]byte("before-" + value)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got := string(b.Bytes())
	if strings.Contains(got, value) {
		t.Fatalf("Bytes() = %q, leaked the registered value", got)
	}
	if !strings.Contains(got, redact.Marker) {
		t.Errorf("Bytes() = %q, want it to contain %q (the retention cut runs on the masked stream)", got, redact.Marker)
	}
}

func TestTailBuffer_MasksAValueSplitAcrossWrites(t *testing.T) {
	t.Parallel()

	value := randomTailBufferSecret(t)
	redact.Add("test.TailBuffer split write", value)

	b := NewTailBuffer(4096)
	half := len(value) / 2
	if _, err := b.Write([]byte("start-" + value[:half])); err != nil {
		t.Fatalf("Write(first half) error = %v", err)
	}
	if _, err := b.Write([]byte(value[half:] + "-end")); err != nil {
		t.Fatalf("Write(second half) error = %v", err)
	}

	got := string(b.Bytes())
	if strings.Contains(got, value) {
		t.Fatalf("Bytes() = %q, still contains a value split across two Write calls", got)
	}
	if !strings.Contains(got, redact.Marker) {
		t.Errorf("Bytes() = %q, want it to contain %q", got, redact.Marker)
	}
}

func TestTailBuffer_Collector(t *testing.T) {
	t.Parallel()

	value := randomTailBufferSecret(t)
	redact.Add("test.TailBuffer collector", value)

	b := NewTailBuffer(4096)
	if _, err := b.Write([]byte("first line\nsecond line with " + value + "\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	c := b.Collector(nil)
	lines := c.Lines()
	if len(lines) != 2 {
		t.Fatalf("Collector().Lines() = %v, want 2 lines", lines)
	}
	if lines[0] != "first line" {
		t.Errorf("Collector().Lines()[0] = %q, want %q", lines[0], "first line")
	}
	if strings.Contains(lines[1], value) {
		t.Errorf("Collector().Lines()[1] = %q, leaked the registered value", lines[1])
	}
	if !strings.Contains(lines[1], redact.Marker) {
		t.Errorf("Collector().Lines()[1] = %q, want it to contain %q", lines[1], redact.Marker)
	}
}

func TestTailBuffer_Collector_UsesDefaultLoggerWhenNil(t *testing.T) {
	t.Parallel()

	b := NewTailBuffer(64)
	if _, err := b.Write([]byte("output\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	c := b.Collector(nil)
	if got := c.Lines(); len(got) != 1 || got[0] != "output" {
		t.Errorf("Collector(nil).Lines() = %v, want [%q]", got, "output")
	}
}

func TestTailBuffer_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	b := NewTailBuffer(4096)
	const goroutines = 8
	const chunkLen = 16
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			if _, err := b.Write(bytes.Repeat([]byte("x"), chunkLen)); err != nil {
				t.Errorf("Write() error = %v", err)
			}
		})
	}
	wg.Wait()

	if got, want := len(b.Bytes()), goroutines*chunkLen; got != want {
		t.Errorf("total retained bytes = %d, want %d", got, want)
	}
}
