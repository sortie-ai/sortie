package agenttest_test

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestLogSpy_Handle_CapturesEntry(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	logger := slog.New(spy)

	logger.Warn("agent stderr", slog.String("line", "startup rejected"))

	entries := spy.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() = %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Level != slog.LevelWarn {
		t.Errorf("Level = %v, want WARN", e.Level)
	}
	if e.Msg != "agent stderr" {
		t.Errorf("Msg = %q, want \"agent stderr\"", e.Msg)
	}
	if e.Line != "startup rejected" {
		t.Errorf("Line = %q, want \"startup rejected\"", e.Line)
	}
}

func TestLogSpy_Handle_MissingLineAttr(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	logger := slog.New(spy)

	logger.Warn("agent stderr") // no "line" attr

	entries := spy.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() = %d entries, want 1", len(entries))
	}
	if entries[0].Line != "" {
		t.Errorf("Line = %q, want \"\" when attr is absent", entries[0].Line)
	}
}

func TestLogSpy_Handle_OtherAttrsIgnored(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	logger := slog.New(spy)

	logger.Warn("agent stderr", slog.String("other", "value"), slog.String("line", "captured"))

	entries := spy.Entries()
	if len(entries) == 0 {
		t.Fatal("Entries() is empty, want one entry")
	}
	if entries[0].Line != "captured" {
		t.Errorf("Line = %q, want \"captured\"", entries[0].Line)
	}
}

func TestLogSpy_Handle_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	logger := slog.New(spy)
	const goroutines = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			logger.Warn("agent stderr", slog.String("line", "concurrent line"))
		}()
	}
	wg.Wait()

	if got := len(spy.Entries()); got != goroutines {
		t.Errorf("Entries() = %d, want %d", got, goroutines)
	}
}

func TestLogSpy_WithAttrs_ReturnsSelf(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	got := spy.WithAttrs([]slog.Attr{slog.String("k", "v")})

	if got != spy {
		t.Error("WithAttrs() returned a different handler, want same receiver")
	}
}

func TestLogSpy_WithGroup_ReturnsSelf(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	got := spy.WithGroup("grp")

	if got != spy {
		t.Error("WithGroup() returned a different handler, want same receiver")
	}
}

// TestLogSpy_WarnLines checks the dual-condition filter: level==WARN AND
// msg=="agent stderr". Entries that differ on either condition must be excluded.
func TestLogSpy_WarnLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(l *slog.Logger)
		wantLen  int
		wantLine string // first expected line, empty if wantLen == 0
	}{
		{
			name:    "no entries yields empty slice",
			setup:   func(_ *slog.Logger) {},
			wantLen: 0,
		},
		{
			name:     "WARN agent-stderr captured",
			setup:    func(l *slog.Logger) { l.Warn("agent stderr", slog.String("line", "bad exit")) },
			wantLen:  1,
			wantLine: "bad exit",
		},
		{
			name:    "DEBUG agent-stderr not captured",
			setup:   func(l *slog.Logger) { l.Debug("agent stderr", slog.String("line", "debug line")) },
			wantLen: 0,
		},
		{
			name:    "WARN with different message not captured",
			setup:   func(l *slog.Logger) { l.Warn("agent stdout", slog.String("line", "stdout line")) },
			wantLen: 0,
		},
		{
			name: "mixed entries: only matching ones returned",
			setup: func(l *slog.Logger) {
				l.Debug("agent stderr", slog.String("line", "debug"))
				l.Warn("agent stderr", slog.String("line", "warn-match"))
				l.Warn("other message", slog.String("line", "other"))
			},
			wantLen:  1,
			wantLine: "warn-match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spy := &agenttest.LogSpy{}
			tt.setup(slog.New(spy))

			got := spy.WarnLines()
			if len(got) != tt.wantLen {
				t.Fatalf("WarnLines() = %v (len %d), want len %d", got, len(got), tt.wantLen)
			}
			if tt.wantLine != "" && (len(got) == 0 || got[0] != tt.wantLine) {
				t.Errorf("WarnLines()[0] = %q, want %q", got[0], tt.wantLine)
			}
		})
	}
}

func TestLogSpy_Entries_IsSnapshot(t *testing.T) {
	t.Parallel()

	spy := &agenttest.LogSpy{}
	logger := slog.New(spy)

	logger.Warn("agent stderr", slog.String("line", "first"))
	snap1 := spy.Entries()

	logger.Warn("agent stderr", slog.String("line", "second"))
	snap2 := spy.Entries()

	if len(snap1) != 1 {
		t.Errorf("first snapshot len = %d, want 1", len(snap1))
	}
	if len(snap2) != 2 {
		t.Errorf("second snapshot len = %d, want 2", len(snap2))
	}
}

func TestInstallLogSpy_SetsDefault(t *testing.T) {
	// Not parallel — manipulates global slog.Default.
	spy := agenttest.InstallLogSpy(t)

	slog.Default().Warn("agent stderr", slog.String("line", "global log"))

	entries := spy.Entries()
	if len(entries) == 0 {
		t.Fatal("spy captured no entries after logging via slog.Default()")
	}
	if entries[0].Msg != "agent stderr" {
		t.Errorf("Msg = %q, want \"agent stderr\"", entries[0].Msg)
	}
}

func TestRequireWarnLines_ReturnsLines(t *testing.T) {
	t.Parallel()

	spy := agenttest.InstallLogSpy(t)
	slog.Default().Warn("agent stderr", slog.String("line", "startup rejected: no license"))

	lines := agenttest.RequireWarnLines(t, spy, "non-zero exit")
	if len(lines) != 1 {
		t.Fatalf("RequireWarnLines() = %v, want 1 line", lines)
	}
	if lines[0] != "startup rejected: no license" {
		t.Errorf("lines[0] = %q, want \"startup rejected: no license\"", lines[0])
	}
}
