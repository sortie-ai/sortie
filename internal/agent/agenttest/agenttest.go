// Package agenttest provides shared test helpers for agent adapter tests.
package agenttest

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// WriteScript writes an executable shell script with the given content to
// dir/name and returns the absolute path.
//
// The script is written via a child process to avoid the ETXTBSY race on
// Linux (golang/go#22315). In a multithreaded process, fork() duplicates all
// parent file descriptors into the child. If one goroutine holds a write FD on
// a file while another goroutine forks for an unrelated exec, the forked child
// inherits that write FD. Even after the parent closes the FD, the child
// retains it until exec() closes CLOEXEC descriptors. Any attempt to exec the
// file during that window fails with ETXTBSY because the kernel sees
// i_writecount > 0 on the inode. Delegating the write to a subprocess means
// the parent never opens a write FD on the executable, eliminating the race at
// its root rather than masking it with retries.
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
