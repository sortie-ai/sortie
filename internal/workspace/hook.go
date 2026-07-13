package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// MaxHookOutputBytes is the maximum number of bytes retained from a
// hook's combined stdout and stderr. When output exceeds this cap the
// earliest bytes are dropped so the retained value holds the tail,
// where a failing hook's error message almost always is. The cap is
// deliberately small so the captured output, emitted verbatim as one
// structured log field, stays under the single-line size limit that
// common log collectors impose before they split a record.
const MaxHookOutputBytes = 8 * 1024

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
// code 0). Output contains the combined stdout and stderr, retained as
// the last [MaxHookOutputBytes] bytes.
type HookResult struct {
	// Output is the combined stdout+stderr of the hook: the last
	// [MaxHookOutputBytes] bytes, prefixed with a truncation marker
	// when earlier output was dropped.
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

// limitedBuffer retains the last max bytes written to it, dropping the
// earliest bytes once the total exceeds max. It implements [io.Writer]
// for use as cmd.Stdout and cmd.Stderr and is safe for concurrent use.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

// Write appends p and, once the retained content exceeds max, discards
// the earliest bytes so only the most recent max bytes remain. It
// always returns len(p), nil to prevent [os/exec.Cmd] short-write
// errors, and is safe for concurrent use.
func (lb *limitedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	// A single write larger than the cap overwrites the whole retained
	// window, so keep only its last max bytes rather than growing the
	// buffer to hold all of p and trimming afterward.
	if len(p) > lb.max {
		lb.buf.Reset()
		lb.buf.Write(p[len(p)-lb.max:]) //nolint:errcheck // bytes.Buffer.Write never returns an error
		lb.truncated = true
		return len(p), nil
	}

	lb.buf.Write(p) //nolint:errcheck // bytes.Buffer.Write never returns an error
	if overflow := lb.buf.Len() - lb.max; overflow > 0 {
		lb.buf.Next(overflow)
		lb.truncated = true
	}
	return len(p), nil
}

// String returns the retained tail of the written bytes. When earlier
// bytes were dropped, the tail is prefixed with a one-line marker
// reporting the number of bytes shown.
func (lb *limitedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if !lb.truncated {
		return lb.buf.String()
	}
	return fmt.Sprintf("[truncated: showing last %d bytes of hook output]\n%s", lb.max, lb.buf.String())
}

// hookEnv builds a restricted environment for the hook subprocess.
// Only variables in allowedEnvKeys and variables whose name starts
// with "SORTIE_" are inherited from the parent process. Variables in
// override take precedence over same-named parent variables.
func hookEnv(override map[string]string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(allowedEnvKeys)+len(override))
	overrideNorm := make(map[string]bool, len(override))
	for k := range override {
		overrideNorm[normalizeEnvKey(k)] = true
	}
	for _, entry := range parent {
		k, _, _ := strings.Cut(entry, "=")
		norm := normalizeEnvKey(k)
		if !allowedEnvKeys[norm] && !strings.HasPrefix(norm, "SORTIE_") {
			continue
		}
		if overrideNorm[norm] {
			continue
		}
		env = append(env, entry)
	}
	for k, v := range override {
		env = append(env, k+"="+v)
	}
	return env
}

// validateParams checks HookParams preconditions and returns a
// *HookError with Op "validate" on any violation.
func validateParams(params HookParams) error {
	if params.Script == "" {
		return &HookError{
			Op:       "validate",
			Script:   "",
			ExitCode: -1,
			Err:      errors.New("script must not be empty"),
		}
	}

	if params.Dir == "" {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      errors.New("dir must not be empty"),
		}
	}

	info, err := os.Stat(params.Dir)
	if err != nil {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      fmt.Errorf("dir %q: %w", params.Dir, err),
		}
	}
	if !info.IsDir() {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      fmt.Errorf("dir %q: not a directory", params.Dir),
		}
	}

	if params.TimeoutMS <= 0 {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      errors.New("timeout_ms must be positive"),
		}
	}

	return nil
}
