// Package agenttest provides shared test helpers for agent adapter tests.
package agenttest

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RequireSetsid skips t cleanly when the setsid binary is not on PATH.
// An escaped-descendant fixture needs it to detach a background job into
// its own session, so it survives the process-group termination the
// fixture exercises. setsid ships with util-linux and is absent on
// macOS, which keeps a suite failing there for a reason unrelated to
// what it tests.
func RequireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("skipping: setsid not found on PATH: %v", err)
	}
}

// WriteScript writes an executable shell script with the given content to
// dir/name and returns the absolute path.
//
// The script is written via a child process to avoid the ETXTBSY race on
// Linux: in a multithreaded process, a write FD held by one goroutine can
// survive across fork() into a child spawned by another goroutine, causing
// ETXTBSY when the file is exec'd before the inherited descriptor closes.
// Delegating the write to a subprocess means the parent never opens a write FD
// on the executable, eliminating the race at its root. See golang/go#22315.
func WriteScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", `cat > "$1" && chmod 0755 "$1"`, "sh", path) //nolint:gosec // path is always under t.TempDir()
	cmd.Stdin = strings.NewReader("#!/bin/sh\n" + content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("WriteScript: %v\n%s", err, out)
	}
	return path
}
