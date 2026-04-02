//go:build unix

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// allowedEnvKeys lists the parent-process environment variables that
// hook subprocesses are permitted to inherit. All other variables are
// stripped so that secrets (e.g., JIRA_API_TOKEN, cloud credentials)
// are not present in the hook subprocess environment unless explicitly injected.
var allowedEnvKeys = map[string]bool{
	"PATH":          true,
	"HOME":          true,
	"SHELL":         true,
	"TMPDIR":        true,
	"USER":          true,
	"LOGNAME":       true,
	"TERM":          true,
	"LANG":          true,
	"LC_ALL":        true,
	"SSH_AUTH_SOCK": true,
}

// hookEnv builds a restricted environment for the hook subprocess.
// Only variables in [allowedEnvKeys] and variables whose name starts
// with "SORTIE_" are inherited from the parent process. Variables in
// override take precedence over same-named parent variables.
func hookEnv(override map[string]string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(allowedEnvKeys)+len(override))
	for _, entry := range parent {
		k, _, _ := strings.Cut(entry, "=")
		if !allowedEnvKeys[k] && !strings.HasPrefix(k, "SORTIE_") {
			continue
		}
		if _, dup := override[k]; dup {
			continue
		}
		env = append(env, entry)
	}
	for k, v := range override {
		env = append(env, k+"="+v)
	}
	return env
}

// RunHook executes a shell hook script in the specified workspace
// directory, enforcing a timeout and capturing output. The parent
// context ctx allows the caller to cancel the hook independently of
// the timeout (e.g., on graceful shutdown).
//
// The subprocess receives a restricted environment: only standard
// POSIX infrastructure variables and SORTIE_* variables are inherited
// from the parent process. Variables in params.Env are merged last
// and override any same-named parent variable.
//
// On success (exit code 0), returns a [HookResult] with truncated
// output. On failure, returns a [*HookError] with Op indicating the
// failure mode:
//   - "validate": invalid params (empty script, non-directory Dir,
//     non-positive TimeoutMS)
//   - "start": subprocess could not be started (missing shell, etc.)
//   - "run": subprocess exited with non-zero exit code
//   - "timeout": subprocess exceeded TimeoutMS or parent ctx cancelled
//
// Output is always captured and truncated to [MaxHookOutputBytes],
// even on failure, so callers can log diagnostic output.
func RunHook(ctx context.Context, params HookParams) (HookResult, error) {
	if err := validateParams(params); err != nil {
		return HookResult{}, err
	}

	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "sh", "-c", params.Script) //nolint:gosec // G204: hook scripts are from trusted workflow configuration
	cmd.Dir = params.Dir

	// Place the shell and all descendants in a new process group so
	// timeout termination kills the entire tree, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Kill the entire process group when the context expires instead
	// of only the direct child, preventing orphaned grandchildren.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	// Allow child processes time to exit and release I/O pipes after
	// the group signal before Go forcibly closes pipes.
	cmd.WaitDelay = 3 * time.Second

	cmd.Env = hookEnv(params.Env)

	buf := &limitedBuffer{max: MaxHookOutputBytes}
	cmd.Stdout = buf
	cmd.Stderr = buf

	err := cmd.Run()
	output := buf.String()

	if err == nil {
		return HookResult{Output: output}, nil
	}

	// ORDERING INVARIANT: Check context error BEFORE *exec.ExitError.
	// A process killed by SIGKILL (timeout) also produces an ExitError
	// with signal status. Checking context first ensures correct
	// classification. Do not reorder these blocks.
	if hookCtx.Err() == context.DeadlineExceeded {
		return HookResult{}, &HookError{
			Op:       "timeout",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("hook timed out after %dms: %w", params.TimeoutMS, context.DeadlineExceeded),
		}
	}

	if hookCtx.Err() == context.Canceled {
		return HookResult{}, &HookError{
			Op:       "timeout",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("hook cancelled: %w", context.Canceled),
		}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return HookResult{}, &HookError{
			Op:       "run",
			Script:   truncateScript(params.Script),
			ExitCode: exitErr.ExitCode(),
			Output:   output,
			Err:      err,
		}
	}

	return HookResult{}, &HookError{
		Op:       "start",
		Script:   truncateScript(params.Script),
		ExitCode: -1,
		Output:   output,
		Err:      err,
	}
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
