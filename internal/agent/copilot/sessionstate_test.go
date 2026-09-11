package copilot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestSessionStateRoot(t *testing.T) {
	t.Parallel()

	t.Run("COPILOT_HOME set", func(t *testing.T) {
		t.Parallel()
		env := func(key string) string {
			if key == "COPILOT_HOME" {
				return "/custom/copilot/home"
			}
			return ""
		}
		home := func() (string, error) { t.Fatal("home() must not be called when COPILOT_HOME is set"); return "", nil }

		got, err := sessionStateRoot(env, home)
		if err != nil {
			t.Fatalf("sessionStateRoot() error = %v", err)
		}
		want := filepath.Join("/custom/copilot/home", "session-state")
		if got != want {
			t.Errorf("sessionStateRoot() = %q, want %q", got, want)
		}
	})

	t.Run("COPILOT_HOME unset falls back to user home", func(t *testing.T) {
		t.Parallel()
		env := func(string) string { return "" }
		home := func() (string, error) { return "/home/operator", nil }

		got, err := sessionStateRoot(env, home)
		if err != nil {
			t.Fatalf("sessionStateRoot() error = %v", err)
		}
		want := filepath.Join("/home/operator", ".copilot", "session-state")
		if got != want {
			t.Errorf("sessionStateRoot() = %q, want %q", got, want)
		}
	})

	t.Run("home resolution failure propagates", func(t *testing.T) {
		t.Parallel()
		env := func(string) string { return "" }
		wantErr := errors.New("no home directory")
		home := func() (string, error) { return "", wantErr }

		_, err := sessionStateRoot(env, home)
		if !errors.Is(err, wantErr) {
			t.Errorf("sessionStateRoot() error = %v, want wrapping %v", err, wantErr)
		}
	})
}

// writeEventsFile writes content to <dir>/events.jsonl and returns the
// path.
func writeEventsFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

func shutdownLine(inputTokens, outputTokens, cacheReadTokens int64) string {
	return fmt.Sprintf(
		`{"type":"session.shutdown","data":{"modelMetrics":{"claude-sonnet-5":{"usage":{"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d}}}}}`,
		inputTokens, outputTokens, cacheReadTokens,
	)
}

func TestReadSessionUsage(t *testing.T) {
	t.Parallel()

	t.Run("one record: found true, previous zero, model from the sole record", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeEventsFile(t, dir, shutdownLine(100, 20, 5)+"\n")

		current, previous, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		want := domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 5}
		if current != want {
			t.Errorf("current = %+v, want %+v", current, want)
		}
		if previous != (domain.TokenUsage{}) {
			t.Errorf("previous = %+v, want zero (only one record)", previous)
		}
		if model != "claude-sonnet-5" {
			t.Errorf("model = %q, want %q", model, "claude-sonnet-5")
		}
	})

	t.Run("two records: current and previous both populated", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeEventsFile(t, dir, shutdownLine(100, 20, 5)+"\n"+shutdownLine(300, 60, 15)+"\n")

		current, previous, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		wantCurrent := domain.TokenUsage{InputTokens: 300, OutputTokens: 60, TotalTokens: 360, CacheReadTokens: 15}
		if current != wantCurrent {
			t.Errorf("current = %+v, want %+v", current, wantCurrent)
		}
		wantPrevious := domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 5}
		if previous != wantPrevious {
			t.Errorf("previous = %+v, want %+v", previous, wantPrevious)
		}
		if model != "claude-sonnet-5" {
			t.Errorf("model = %q, want %q", model, "claude-sonnet-5")
		}
	})

	t.Run("three records keeps only the last two", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeEventsFile(t, dir,
			shutdownLine(10, 1, 0)+"\n"+
				shutdownLine(100, 20, 5)+"\n"+
				shutdownLine(300, 60, 15)+"\n")

		current, previous, _, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if current.InputTokens != 300 {
			t.Errorf("current.InputTokens = %d, want 300 (last record)", current.InputTokens)
		}
		if previous.InputTokens != 100 {
			t.Errorf("previous.InputTokens = %d, want 100 (second-to-last, not the first record)", previous.InputTokens)
		}
	})

	t.Run("no session.shutdown record", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeEventsFile(t, dir, `{"type":"session.info","data":{}}`+"\n")

		_, _, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if found {
			t.Error("found = true, want false (no session.shutdown record)")
		}
		if model != "" {
			t.Errorf("model = %q, want empty (found = false)", model)
		}
	})

	t.Run("undecodable line is skipped without failing the read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeEventsFile(t, dir, "not valid json\n"+shutdownLine(100, 20, 5)+"\n")

		current, _, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true (the valid line must still be read)")
		}
		if current.InputTokens != 100 {
			t.Errorf("current.InputTokens = %d, want 100", current.InputTokens)
		}
		if model != "claude-sonnet-5" {
			t.Errorf("model = %q, want %q", model, "claude-sonnet-5")
		}
	})

	t.Run("file exceeding the byte cap is rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "events.jsonl")
		f, err := os.Create(path) //nolint:gosec // test-controlled path under t.TempDir()
		if err != nil {
			t.Fatalf("os.Create: %v", err)
		}
		if err := f.Truncate(maxSessionStateFileBytes + 1); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		_, _, _, _, err = readSessionUsage(path)
		if !errors.Is(err, errSessionStateCapExceeded) {
			t.Errorf("readSessionUsage() error = %v, want wrapping errSessionStateCapExceeded", err)
		}
	})

	t.Run("single line exceeding the line cap is rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		longLine := strings.Repeat("x", maxSessionStateLineBytes+1)
		path := writeEventsFile(t, dir, longLine+"\n")

		_, _, _, _, err := readSessionUsage(path)
		if !errors.Is(err, errSessionStateCapExceeded) {
			t.Errorf("readSessionUsage() error = %v, want wrapping errSessionStateCapExceeded", err)
		}
	})

	t.Run("missing file returns an error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, _, _, _, err := readSessionUsage(filepath.Join(dir, "does-not-exist.jsonl"))
		if err == nil {
			t.Fatal("readSessionUsage(missing file) error = nil, want non-nil")
		}
	})

	t.Run("modelMetrics preferred over tokenDetails fallback", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		line := `{"type":"session.shutdown","data":{` +
			`"tokenDetails":{"input":{"tokenCount":1},"cache_read":{"tokenCount":1},"cache_write":{"tokenCount":1},"output":{"tokenCount":1}},` +
			`"modelMetrics":{"claude-sonnet-5":{"usage":{"inputTokens":193011,"outputTokens":596,"cacheReadTokens":154053}}}` +
			`}}`
		path := writeEventsFile(t, dir, line+"\n")

		current, _, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		want := domain.TokenUsage{InputTokens: 193011, OutputTokens: 596, TotalTokens: 193607, CacheReadTokens: 154053}
		if current != want {
			t.Errorf("current = %+v, want %+v (modelMetrics preferred)", current, want)
		}
		if model != "claude-sonnet-5" {
			t.Errorf("model = %q, want %q", model, "claude-sonnet-5")
		}
	})

	t.Run("tokenDetails fallback when modelMetrics is absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		line := `{"type":"session.shutdown","data":{` +
			`"tokenDetails":{"input":{"tokenCount":10},"cache_read":{"tokenCount":154053},"cache_write":{"tokenCount":38948},"output":{"tokenCount":596}}` +
			`}}`
		path := writeEventsFile(t, dir, line+"\n")

		current, _, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		want := domain.TokenUsage{InputTokens: 193011, OutputTokens: 596, TotalTokens: 193607, CacheReadTokens: 154053}
		if current != want {
			t.Errorf("current = %+v, want %+v (tokenDetails: input+cache_read+cache_write)", current, want)
		}
		if model != "" {
			t.Errorf("model = %q, want empty (no modelMetrics to name one)", model)
		}
	})

	t.Run("tokenDetails fallback when modelMetrics is present but empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		line := `{"type":"session.shutdown","data":{` +
			`"tokenDetails":{"input":{"tokenCount":5},"cache_read":{"tokenCount":2},"cache_write":{"tokenCount":1},"output":{"tokenCount":3}},` +
			`"modelMetrics":{}` +
			`}}`
		path := writeEventsFile(t, dir, line+"\n")

		current, _, model, found, err := readSessionUsage(path)
		if err != nil {
			t.Fatalf("readSessionUsage() error = %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		want := domain.TokenUsage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11, CacheReadTokens: 2}
		if current != want {
			t.Errorf("current = %+v, want %+v (empty modelMetrics falls back to tokenDetails)", current, want)
		}
		if model != "" {
			t.Errorf("model = %q, want empty (empty modelMetrics names no model)", model)
		}
	})
}

// TestShutdownModelName pins shutdownModelName's selection rule directly
// against shutdownEvent values, independent of readSessionUsage's own
// line-scanning and record-window logic.
func TestShutdownModelName(t *testing.T) {
	t.Parallel()

	metrics := func(pairs map[string][2]int64) map[string]shutdownModel {
		out := make(map[string]shutdownModel, len(pairs))
		for name, io := range pairs {
			out[name] = shutdownModel{Usage: shutdownModelUsage{InputTokens: io[0], OutputTokens: io[1]}}
		}
		return out
	}

	tests := []struct {
		name     string
		current  shutdownEvent
		previous shutdownEvent
		want     string
	}{
		{
			name: "largest positive growth wins",
			current: shutdownEvent{Data: shutdownData{ModelMetrics: metrics(map[string][2]int64{
				"claude-sonnet-5": {200, 50},
				"gpt-6-astra":     {120, 10},
			})}},
			previous: shutdownEvent{Data: shutdownData{ModelMetrics: metrics(map[string][2]int64{
				"claude-sonnet-5": {100, 20},
				"gpt-6-astra":     {110, 9},
			})}},
			want: "claude-sonnet-5",
		},
		{
			name: "equal growth resolves to the name that sorts first",
			current: shutdownEvent{Data: shutdownData{ModelMetrics: metrics(map[string][2]int64{
				"zeta-model":  {130, 0},
				"alpha-model": {130, 0},
			})}},
			previous: shutdownEvent{},
			want:     "alpha-model",
		},
		{
			name: "a key blank after trimming is skipped",
			current: shutdownEvent{Data: shutdownData{ModelMetrics: metrics(map[string][2]int64{
				"   ":             {999, 999},
				"claude-sonnet-5": {50, 5},
			})}},
			previous: shutdownEvent{},
			want:     "claude-sonnet-5",
		},
		{
			name: "no positive growth returns empty",
			current: shutdownEvent{Data: shutdownData{ModelMetrics: metrics(map[string][2]int64{
				"claude-sonnet-5": {100, 20},
			})}},
			previous: shutdownEvent{Data: shutdownData{ModelMetrics: metrics(map[string][2]int64{
				"claude-sonnet-5": {100, 20},
			})}},
			want: "",
		},
		{
			name:     "empty modelMetrics returns empty",
			current:  shutdownEvent{},
			previous: shutdownEvent{},
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shutdownModelName(tt.current, tt.previous)
			if got != tt.want {
				t.Errorf("shutdownModelName() = %q, want %q", got, tt.want)
			}
		})
	}
}
