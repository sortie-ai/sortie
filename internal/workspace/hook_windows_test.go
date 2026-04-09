//go:build windows

package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireHookError(t *testing.T, err error) *HookError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *HookError, got nil")
	}
	var he *HookError
	if !errors.As(err, &he) {
		t.Fatalf("error type = %T, want *HookError", err)
	}
	return he
}

func TestRunHook_HappyPath(t *testing.T) {
	t.Parallel()

	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo hello",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "hello")
	}
}

func TestRunHook_EnvVars(t *testing.T) {
	t.Parallel()

	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo %SORTIE_FOO%",
		Dir:       t.TempDir(),
		Env:       map[string]string{"SORTIE_FOO": "bar"},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}
	if !strings.Contains(result.Output, "bar") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "bar")
	}
}

func TestRunHook_Cwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// EvalSymlinks resolves any symlinks in the temp path.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}

	result, err := RunHook(context.Background(), HookParams{
		Script:    "echo %CD%",
		Dir:       dir,
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}

	got := strings.TrimSpace(result.Output)
	// Normalise both sides to uppercase for case-insensitive comparison.
	if !strings.EqualFold(got, realDir) {
		t.Errorf("%%CD%% = %q, want %q", got, realDir)
	}
}

func TestRunHook_NonZeroExit(t *testing.T) {
	t.Parallel()

	_, err := RunHook(context.Background(), HookParams{
		Script:    "exit 1",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})

	he := requireHookError(t, err)
	if he.Op != "run" {
		t.Errorf("HookError.Op = %q, want %q", he.Op, "run")
	}
	if he.ExitCode != 1 {
		t.Errorf("HookError.ExitCode = %d, want 1", he.ExitCode)
	}
}

func TestRunHook_Timeout(t *testing.T) {
	t.Parallel()

	// "pause" waits for a key press indefinitely — guaranteed to time out.
	_, err := RunHook(context.Background(), HookParams{
		Script:    "pause",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 200,
	})

	he := requireHookError(t, err)
	if he.Op != "timeout" {
		t.Errorf("HookError.Op = %q, want %q", he.Op, "timeout")
	}
	if he.ExitCode != -1 {
		t.Errorf("HookError.ExitCode = %d, want -1", he.ExitCode)
	}
}

func TestRunHook_ProcessTreeKill(t *testing.T) {
	t.Parallel()

	// The script spawns a detached background child via "start /b" then
	// blocks itself. The Job Object created in RunHook ensures both the
	// outer cmd.exe and its child are terminated when the timeout fires.
	script := "start /b cmd.exe /C pause & pause"

	start := time.Now()
	_, err := RunHook(context.Background(), HookParams{
		Script:    script,
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 300,
	})

	he := requireHookError(t, err)
	if he.Op != "timeout" {
		t.Errorf("HookError.Op = %q, want %q", he.Op, "timeout")
	}

	// RunHook must return promptly after the timeout — allow generous
	// slack for WaitDelay (3 s) plus scheduling jitter.
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("RunHook took %v after timeout; expected prompt return (< 5s)", elapsed)
	}
}
