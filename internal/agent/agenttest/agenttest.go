// Package agenttest provides shared test helpers for agent adapter tests.
package agenttest

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RequireSetsid skips t cleanly when setsid is not on PATH. An
// escaped-descendant fixture needs it to detach a background job into its own
// session; setsid ships with util-linux and is absent on macOS.
func RequireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("skipping: setsid not found on PATH: %v", err)
	}
}

// WriteScript writes an executable shell script with content to dir/name and
// returns the path.
//
// The write is delegated to a child process so the parent never opens a write
// FD on the executable: an inherited write FD surviving a fork by another
// goroutine causes ETXTBSY when the file is exec'd. See golang/go#22315.
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

// WriteRecordingScript writes an executable shell script as [WriteScript] does
// and has it append its background jobs' process ids to its [DescendantReceipt]
// as it exits. Only the shell can name those jobs once it is gone, so a script
// killed outright records nothing.
func WriteRecordingScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	record := "__agenttest_record_descendants() { jobs -p >> " + shellWord(DescendantReceipt(filepath.Join(dir, name))) + "; }\n" +
		"trap __agenttest_record_descendants EXIT\n"
	return WriteScript(t, dir, name, record+content)
}

func shellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
