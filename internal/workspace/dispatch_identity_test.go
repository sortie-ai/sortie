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

// slogCapture returns a slog.Logger backed by an in-memory buffer at
// warn level and a function retrieving the captured output.
func slogCapture() (*slog.Logger, func() string) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(h), func() string { return buf.String() }
}

// mkSortieDir creates <ws>/.sortie so [WriteDispatchIdentity], which
// never creates it, has somewhere to write.
func mkSortieDir(t *testing.T, ws string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}
}

// writeRawDispatchRecord writes raw bytes directly to
// <ws>/.sortie/dispatch.json, bypassing WriteSortieFile, so a test can
// plant malformed or oversized content.
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
	mkSortieDir(t, ws)
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
		mkSortieDir(t, ws)
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
		mkSortieDir(t, ws)
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
		mkSortieDir(t, ws)
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

// TestReadDispatchSessionID_MismatchFailsWithoutTheComparison proves the
// mismatch case is load-bearing: with the DispatchID comparison removed
// (simulated here by asking for the record's own dispatch id, which
// would incorrectly succeed if ReadDispatchSessionID returned
// record.SessionID unconditionally), the exact-match test above is what
// would catch a broken comparison. This test documents the mismatch
// case's own assertion is not vacuously true: a record does exist and
// does carry a session id, so an implementation that ignored the
// dispatch id argument would return "S1" here instead of "".
func TestReadDispatchSessionID_MismatchFailsWithoutTheComparison(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	mkSortieDir(t, ws)
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
		// 4097 bytes: one past maxDispatchIdentityBytes.
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

// assertWarnReason fails the test unless log contains the documented
// warning message and the given reason attribute.
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
