//go:build unix

package probe

import (
	"strings"
	"testing"
)

func TestPromptNamingProbe(t *testing.T) {
	t.Parallel()

	fixture := &sharedFixture{failingProbe: "/tmp/x/failing-probe"}
	got := promptNamingProbe(fixture, "failing")
	if !strings.Contains(got, fixture.failingProbe) {
		t.Errorf("promptNamingProbe(...) = %q, want it to name %q", got, fixture.failingProbe)
	}
}

func writeAll(t *testing.T, w *lineBoundedWriter, writes []string) {
	t.Helper()
	for _, chunk := range writes {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q) error = %v, want nil", chunk, err)
		}
		if n != len(chunk) {
			t.Fatalf("Write(%q) = %d, want %d", chunk, n, len(chunk))
		}
	}
}

func TestLineBoundedWriter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		limit       int
		writes      []string
		flush       bool
		wantString  string
		wantDropped int
		wantPeak    int
	}{
		{
			name:       "a single write carrying two complete lines retains both",
			limit:      100,
			writes:     []string{"line1\nline2\n"},
			wantString: "line1\nline2\n",
			wantPeak:   len("line1\n"),
		},
		{
			name:       "a line split across two writes reassembles on the second",
			limit:      100,
			writes:     []string{"li", "ne1\n"},
			wantString: "line1\n",
			wantPeak:   len("line1\n"),
		},
		{
			name:        "a line found to exceed limit within one write is dropped before ever reaching the buffer",
			limit:       5,
			writes:      []string{"abcdefgh\n"},
			wantString:  "",
			wantDropped: 1,
			wantPeak:    0,
		},
		{
			name:        "a dropped oversized line does not affect a later line in the same write",
			limit:       5,
			writes:      []string{"abcdefgh\nok\n"},
			wantString:  "ok\n",
			wantDropped: 1,
			wantPeak:    len("ok\n"),
		},
		{
			name:        "a write exceeding limit with no newline yet discards through the write that supplies the newline",
			limit:       5,
			writes:      []string{"abcdefgh", "ijk\nok\n"},
			wantString:  "ok\n",
			wantDropped: 1,
			wantPeak:    len("ok\n"),
		},
		{
			name:        "discarding spans more than one write while none of them carries a newline",
			limit:       5,
			writes:      []string{"abcdefgh", "more-no-newline-either", "ijk\nok\n"},
			wantString:  "ok\n",
			wantDropped: 1,
			wantPeak:    len("ok\n"),
		},
		{
			name:       "a trailing partial line is retained only after Flush",
			limit:      100,
			writes:     []string{"partial-no-newline"},
			flush:      true,
			wantString: "partial-no-newline",
			wantPeak:   len("partial-no-newline"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := &lineBoundedWriter{limit: tt.limit}
			writeAll(t, w, tt.writes)
			if tt.flush {
				w.Flush()
			}

			if got := w.String(); got != tt.wantString {
				t.Errorf("String() = %q, want %q", got, tt.wantString)
			}
			if w.dropped != tt.wantDropped {
				t.Errorf("dropped = %d, want %d", w.dropped, tt.wantDropped)
			}
			if w.peak != tt.wantPeak {
				t.Errorf("peak = %d, want %d", w.peak, tt.wantPeak)
			}
		})
	}
}

func TestLineBoundedWriterPeak(t *testing.T) {
	t.Parallel()

	t.Run("peak reaches the full reassembled line length", func(t *testing.T) {
		t.Parallel()

		w := &lineBoundedWriter{limit: 100}
		writeAll(t, w, []string{"abc", "defgh\n"})
		if w.peak != len("abcdefgh\n") {
			t.Errorf("peak = %d, want %d", w.peak, len("abcdefgh\n"))
		}
	})

	t.Run("a write dropped before buffering leaves peak at zero", func(t *testing.T) {
		t.Parallel()

		w := &lineBoundedWriter{limit: 5}
		writeAll(t, w, []string{"abcdefghij"})
		if w.peak != 0 {
			t.Errorf("peak = %d, want 0: the oversized write must never reach buffer", w.peak)
		}
	})
}
