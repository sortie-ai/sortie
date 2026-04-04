package agenttest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

// LogSpyEntry holds a single log record captured by [LogSpy].
type LogSpyEntry struct {
	Level slog.Level
	Msg   string
	Line  string // value of the "line" slog.Attr, if present
}

// LogSpy is a [slog.Handler] that records every log record. It returns
// itself from WithAttrs and WithGroup so that all loggers derived from
// a spy-backed [slog.Logger] funnel into the same record slice.
type LogSpy struct {
	mu      sync.Mutex
	entries []LogSpyEntry
}

// Enabled reports true for all levels so every record is captured.
func (s *LogSpy) Enabled(_ context.Context, _ slog.Level) bool { return true }

// Handle records the log entry, extracting the "line" attribute if present.
func (s *LogSpy) Handle(_ context.Context, r slog.Record) error {
	e := LogSpyEntry{Level: r.Level, Msg: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "line" {
			e.Line = a.Value.String()
		}
		return true
	})
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.mu.Unlock()
	return nil
}

// WithAttrs returns the receiver unchanged so derived loggers share
// the same entry slice.
func (s *LogSpy) WithAttrs(_ []slog.Attr) slog.Handler { return s }

// WithGroup returns the receiver unchanged so derived loggers share
// the same entry slice.
func (s *LogSpy) WithGroup(_ string) slog.Handler { return s }

// WarnLines returns the "line" attribute values from every record
// logged at WARN with message "agent stderr".
func (s *LogSpy) WarnLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		if e.Level == slog.LevelWarn && e.Msg == "agent stderr" {
			out = append(out, e.Line)
		}
	}
	return out
}

// Entries returns a snapshot of all captured log entries.
func (s *LogSpy) Entries() []LogSpyEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]LogSpyEntry, len(s.entries))
	copy(cp, s.entries)
	return cp
}

// InstallLogSpy replaces [slog.Default] with a spy logger for the
// duration of the test. The original default is restored via
// [testing.T.Cleanup].
func InstallLogSpy(t *testing.T) *LogSpy {
	t.Helper()
	spy := &LogSpy{}
	orig := slog.Default()
	slog.SetDefault(slog.New(spy))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return spy
}

// RequireWarnLines asserts that spy captured at least one WARN
// "agent stderr" line and returns the matched lines. On failure it
// dumps all captured entries to aid CI debugging.
func RequireWarnLines(t *testing.T, spy *LogSpy, label string) []string {
	t.Helper()
	lines := spy.WarnLines()
	if len(lines) > 0 {
		return lines
	}
	entries := spy.Entries()
	t.Fatalf("no WARN lines emitted for stderr on %s; spy captured %d entries: %s",
		label, len(entries), formatEntries(entries))
	return nil // unreachable
}

func formatEntries(entries []LogSpyEntry) string {
	if len(entries) == 0 {
		return "(none)"
	}
	var s string
	for i, e := range entries {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("{%s %q line=%q}", e.Level, e.Msg, e.Line)
	}
	return s
}
