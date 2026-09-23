//go:build unix

package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// TestPrepare_LeftoverProcessLogsOneRecord pins that a before_run hook
// that exits on its own while leaving a background process in its tree
// makes Prepare return no error and log exactly one
// LeftoversTerminatedMessage record carrying hook and workspace; a
// hook that leaves nothing behind logs no such record.
func TestPrepare_LeftoverProcessLogsOneRecord(t *testing.T) {
	t.Run("leftover process logs one record", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := captureLoggerAt(&buf, slog.LevelInfo)

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          t.TempDir(),
			Identifier:    "PROJ-leftover",
			BeforeRun:     "sleep 30 >/dev/null 2>&1 & exit 0",
			HookTimeoutMS: 5000,
			Logger:        logger,
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}

		out := buf.String()
		if count := strings.Count(out, procutil.LeftoversTerminatedMessage); count != 1 {
			t.Fatalf("log contains %d occurrences of %q, want exactly 1; log = %q", count, procutil.LeftoversTerminatedMessage, out)
		}
		if !strings.Contains(out, "hook=before_run") {
			t.Errorf("log missing hook=before_run attribute; log = %q", out)
		}
		if !strings.Contains(out, "workspace="+result.Path) {
			t.Errorf("log missing workspace=%s attribute; log = %q", result.Path, out)
		}
	})

	t.Run("no leftover process logs no record", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := captureLoggerAt(&buf, slog.LevelInfo)

		_, err := Prepare(context.Background(), PrepareParams{
			Root:          t.TempDir(),
			Identifier:    "PROJ-clean",
			BeforeRun:     "exit 0",
			HookTimeoutMS: 5000,
			Logger:        logger,
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}

		if out := buf.String(); strings.Contains(out, procutil.LeftoversTerminatedMessage) {
			t.Errorf("log unexpectedly contains %q; log = %q", procutil.LeftoversTerminatedMessage, out)
		}
	})
}
