package workspace

import (
	"bytes"
	"sync"
)

// MaxHookOutputBytes is the maximum number of bytes captured from
// hook stdout+stderr combined. Output beyond this limit is silently
// discarded. This prevents a runaway hook from consuming unbounded
// memory.
const MaxHookOutputBytes = 256 * 1024

// maxScriptDisplayLen is the maximum number of bytes of a hook script
// included in error messages.
const maxScriptDisplayLen = 200

// HookParams holds the inputs for a single hook invocation.
type HookParams struct {
	// Script is the shell script body to execute via "sh -c".
	// Must be non-empty.
	Script string

	// Dir is the absolute workspace directory path used as the
	// subprocess cwd. Must exist and be a directory.
	Dir string

	// Env holds the SORTIE_* environment variables injected into the
	// hook subprocess. The map is populated by the caller; [RunHook]
	// does not modify or extend it.
	Env map[string]string

	// TimeoutMS is the maximum execution time in milliseconds.
	// Sourced from [config.HooksConfig] TimeoutMS (default 60000).
	// Must be positive.
	TimeoutMS int
}

// HookResult holds the outcome of a successful hook execution (exit
// code 0). Output contains the combined stdout and stderr, truncated
// to [MaxHookOutputBytes].
type HookResult struct {
	// Output is the combined stdout+stderr of the hook, truncated to
	// [MaxHookOutputBytes].
	Output string
}

// truncateScript returns s unchanged if it fits within
// maxScriptDisplayLen bytes, otherwise returns the first
// maxScriptDisplayLen bytes followed by "...".
func truncateScript(s string) string {
	if len(s) <= maxScriptDisplayLen {
		return s
	}
	return s[:maxScriptDisplayLen] + "..."
}

// limitedBuffer captures up to max bytes, silently discarding the
// rest. Implements [io.Writer] for use as cmd.Stdout/Stderr.
// Safe for concurrent use.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

// Write appends p to the buffer up to the configured maximum. Bytes
// beyond the cap are silently discarded. Always returns len(p), nil
// to prevent [os/exec.Cmd] short-write errors.
func (lb *limitedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	remaining := lb.max - lb.buf.Len()
	if remaining > 0 {
		write := p
		if len(write) > remaining {
			write = write[:remaining]
		}
		lb.buf.Write(write) //nolint:errcheck // bytes.Buffer.Write never returns an error
	}
	return len(p), nil
}

func (lb *limitedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
}
