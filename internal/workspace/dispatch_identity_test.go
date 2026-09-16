package workspace

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func slogCapture() (*slog.Logger, func() string) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(h), func() string { return buf.String() }
}

func createSortieDir(t *testing.T, ws string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}
}

// Bypasses WriteSortieFile to create invalid dispatch-record fixtures.
func writeRawDispatchRecord(t *testing.T, ws string, data []byte) {
	t.Helper()
	dir := filepath.Join(ws, sortieDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dispatchIdentityFile), data, 0o600); err != nil {
		t.Fatalf("WriteFile(dispatch.json): %v", err)
	}
}

func TestWriteDispatchIdentity_RoundTrip(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}

	want := DispatchIdentity{DispatchID: "dispatch-abc", SessionID: "sess-xyz"}
	if err := WriteDispatchIdentity(ws, want); err != nil {
		t.Fatalf("WriteDispatchIdentity: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(ws, sortieDir, dispatchIdentityFile))
	if err != nil {
		t.Fatalf("ReadFile(dispatch.json): %v", err)
	}
	var got DispatchIdentity
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal(dispatch.json): %v", err)
	}
	if got != want {
		t.Errorf("decoded record = %+v, want %+v", got, want)
	}
}

func TestReadDispatchSessionID_ExactMatch(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	createSortieDir(t, ws)
	if err := WriteDispatchIdentity(ws, DispatchIdentity{DispatchID: "D1", SessionID: "S1"}); err != nil {
		t.Fatalf("WriteDispatchIdentity: %v", err)
	}

	logger, getLog := slogCapture()
	got := ReadDispatchSessionID(ws, "D1", logger)
	if got != "S1" {
		t.Errorf("ReadDispatchSessionID(match) = %q, want %q", got, "S1")
	}
	if getLog() != "" {
		t.Errorf("ReadDispatchSessionID(match) logged %q, want nothing", getLog())
	}
}

func TestReadDispatchSessionID_SilentEmptyCases(t *testing.T) {
	t.Parallel()

	t.Run("empty dispatchID", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		createSortieDir(t, ws)
		if err := WriteDispatchIdentity(ws, DispatchIdentity{DispatchID: "D1", SessionID: "S1"}); err != nil {
			t.Fatalf("WriteDispatchIdentity: %v", err)
		}
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(empty dispatchID) = %q, want empty", got)
		}
		if getLog() != "" {
			t.Errorf("ReadDispatchSessionID(empty dispatchID) logged %q, want nothing", getLog())
		}
	})

	t.Run("empty workspacePath", func(t *testing.T) {
		t.Parallel()
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID("", "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(empty workspacePath) = %q, want empty", got)
		}
		if getLog() != "" {
			t.Errorf("ReadDispatchSessionID(empty workspacePath) logged %q, want nothing", getLog())
		}
	})

	t.Run("absent record", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
			t.Fatalf("Mkdir(.sortie): %v", err)
		}
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(absent record) = %q, want empty", got)
		}
		if getLog() != "" {
			t.Errorf("ReadDispatchSessionID(absent record) logged %q, want nothing", getLog())
		}
	})

	t.Run("absent .sortie", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(absent .sortie) = %q, want empty", got)
		}
		if getLog() != "" {
			t.Errorf("ReadDispatchSessionID(absent .sortie) logged %q, want nothing", getLog())
		}
	})

	t.Run("mismatched dispatch id", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		createSortieDir(t, ws)
		if err := WriteDispatchIdentity(ws, DispatchIdentity{DispatchID: "D1", SessionID: "S1"}); err != nil {
			t.Fatalf("WriteDispatchIdentity: %v", err)
		}
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D2", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(mismatch) = %q, want empty", got)
		}
		if getLog() != "" {
			t.Errorf("ReadDispatchSessionID(mismatch) logged %q, want nothing", getLog())
		}
	})

	t.Run("empty record dispatch id", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		createSortieDir(t, ws)
		if err := WriteDispatchIdentity(ws, DispatchIdentity{DispatchID: "", SessionID: "S1"}); err != nil {
			t.Fatalf("WriteDispatchIdentity: %v", err)
		}
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(empty record dispatch id) = %q, want empty", got)
		}
		if getLog() != "" {
			t.Errorf("ReadDispatchSessionID(empty record dispatch id) logged %q, want nothing", getLog())
		}
	})
}

func TestReadDispatchSessionID_RejectsMismatchedDispatchID(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	createSortieDir(t, ws)
	if err := WriteDispatchIdentity(ws, DispatchIdentity{DispatchID: "D1", SessionID: "S1"}); err != nil {
		t.Fatalf("WriteDispatchIdentity: %v", err)
	}

	got := ReadDispatchSessionID(ws, "not-D1", nil)
	if got != "" {
		t.Errorf("ReadDispatchSessionID(mismatched dispatchID) = %q, want empty (a record exists with a non-empty session id)", got)
	}
}

func TestReadDispatchSessionID_RejectedCasesWarn(t *testing.T) {
	t.Parallel()

	t.Run("symlink at .sortie", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		target := t.TempDir()
		mustSymlink(t, target, filepath.Join(ws, sortieDir))

		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(.sortie symlink) = %q, want empty", got)
		}
		assertWarnReason(t, getLog(), "symlink")
	})

	t.Run("symlink at record", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
			t.Fatalf("Mkdir(.sortie): %v", err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, []byte(`{"dispatch_id":"D1","session_id":"S1"}`), 0o600); err != nil {
			t.Fatalf("WriteFile(target): %v", err)
		}
		mustSymlink(t, target, filepath.Join(ws, sortieDir, dispatchIdentityFile))

		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(record symlink) = %q, want empty", got)
		}
		assertWarnReason(t, getLog(), "symlink")
	})

	t.Run("directory at record path", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, sortieDir, dispatchIdentityFile), 0o750); err != nil {
			t.Fatalf("MkdirAll(record as directory): %v", err)
		}

		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(directory at record) = %q, want empty", got)
		}
		assertWarnReason(t, getLog(), "not_regular")
	})

	t.Run("oversized record", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		padding := strings.Repeat("a", maxDispatchIdentityBytes+1-len(`{"dispatch_id":"D1","session_id":"S1","pad":""}`)+2)
		raw := []byte(`{"dispatch_id":"D1","session_id":"S1","pad":"` + padding + `"}`)
		if len(raw) <= maxDispatchIdentityBytes {
			t.Fatalf("test fixture length %d does not exceed %d", len(raw), maxDispatchIdentityBytes)
		}
		writeRawDispatchRecord(t, ws, raw)

		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(oversized record) = %q, want empty", got)
		}
		assertWarnReason(t, getLog(), "oversized")
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		writeRawDispatchRecord(t, ws, []byte(`{not-json`))

		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(malformed json) = %q, want empty", got)
		}
		assertWarnReason(t, getLog(), "malformed")
	})

	t.Run("not directory at .sortie handled via regular file case above", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, sortieDir), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(.sortie as file): %v", err)
		}
		logger, getLog := slogCapture()
		got := ReadDispatchSessionID(ws, "D1", logger)
		if got != "" {
			t.Errorf("ReadDispatchSessionID(.sortie is a regular file) = %q, want empty", got)
		}
		assertWarnReason(t, getLog(), "not_directory")
	})
}

func assertWarnReason(t *testing.T, log, reason string) {
	t.Helper()
	if !strings.Contains(log, "dispatch identity record unusable") {
		t.Fatalf("log = %q, want to contain %q", log, "dispatch identity record unusable")
	}
	if !strings.Contains(log, "reason="+reason) {
		t.Errorf("log = %q, want reason=%q", log, reason)
	}
}

func TestReadDispatchSessionID_UnknownKeysIgnored(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	writeRawDispatchRecord(t, ws, []byte(`{"dispatch_id":"D1","session_id":"S1","extra_field":"ignored","another":{"nested":true}}`))

	got := ReadDispatchSessionID(ws, "D1", nil)
	if got != "S1" {
		t.Errorf("ReadDispatchSessionID(unknown keys present) = %q, want %q", got, "S1")
	}
}

func TestReadDispatchSessionID_NilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	target := t.TempDir()
	mustSymlink(t, target, filepath.Join(ws, sortieDir))

	got := ReadDispatchSessionID(ws, "D1", nil)
	if got != "" {
		t.Errorf("ReadDispatchSessionID(nil logger, symlink) = %q, want empty", got)
	}
}
