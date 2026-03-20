//go:build unix

package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func assertHookErrorOp(t *testing.T, err error, wantOp string) {
	t.Helper()
	he := requireHookError(t, err)
	if he.Op != wantOp {
		t.Errorf("HookError.Op = %q, want %q", he.Op, wantOp)
	}
}

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

func TestRunHook(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
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
	})

	t.Run("environment variables set", func(t *testing.T) {
		t.Parallel()

		result, err := RunHook(context.Background(), HookParams{
			Script:    "echo $SORTIE_ISSUE_ID",
			Dir:       t.TempDir(),
			Env:       map[string]string{"SORTIE_ISSUE_ID": "PROJ-42"},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if !strings.Contains(result.Output, "PROJ-42") {
			t.Errorf("Output = %q, want it to contain %q", result.Output, "PROJ-42")
		}
	})

	t.Run("cwd is workspace dir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// EvalSymlinks handles /tmp → /private/tmp on macOS.
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", dir, err)
		}

		result, err := RunHook(context.Background(), HookParams{
			Script:    "pwd",
			Dir:       dir,
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		got := strings.TrimSpace(result.Output)
		if got != realDir {
			t.Errorf("pwd output = %q, want %q", got, realDir)
		}
	})

	t.Run("non-zero exit", func(t *testing.T) {
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
			t.Errorf("ExitCode = %d, want 1", he.ExitCode)
		}
	})

	t.Run("timeout kills hook", func(t *testing.T) {
		t.Parallel()

		_, err := RunHook(context.Background(), HookParams{
			Script:    "sleep 60",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 100,
		})

		he := requireHookError(t, err)
		if he.Op != "timeout" {
			t.Errorf("HookError.Op = %q, want %q", he.Op, "timeout")
		}
		if he.ExitCode != -1 {
			t.Errorf("ExitCode = %d, want -1", he.ExitCode)
		}
	})

	// Section 15.4: process group kill prevents orphaned grandchildren
	t.Run("timeout kills child processes", func(t *testing.T) {
		t.Parallel()

		// The script writes its PID (which is also the PGID due to
		// Setpgid) to stdout, then spawns background sleeps and waits.
		script := `echo $$; sleep 600 & sleep 600 & wait`

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

		// Parse the PGID from output. The shell's PID equals the PGID
		// because Setpgid: true makes it the group leader.
		pgidStr := strings.TrimSpace(he.Output)
		// Output may contain multiple lines if sleep printed something;
		// take just the first line which is the echo $$ output.
		if idx := strings.IndexByte(pgidStr, '\n'); idx >= 0 {
			pgidStr = pgidStr[:idx]
		}
		var pgid int
		if _, err := fmt.Sscanf(pgidStr, "%d", &pgid); err != nil {
			t.Fatalf("failed to parse PGID from output %q: %v", he.Output, err)
		}

		// Allow WaitDelay to fully clean up.
		time.Sleep(500 * time.Millisecond)

		// Signal 0 probes process existence without sending a real signal.
		// ESRCH means the process group no longer exists — which is what we want.
		if err := syscall.Kill(-pgid, 0); err == nil {
			t.Error("process group still alive after timeout; expected it to be killed")
		}
	})

	// Section 15.4: output truncation
	t.Run("output truncation", func(t *testing.T) {
		t.Parallel()

		// Generate ~300 KiB of 'A' characters via yes piped through head.
		script := `yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA | head -c 307200`

		result, err := RunHook(context.Background(), HookParams{
			Script:    script,
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 10000,
		})

		// The script exits 0 if head closes the pipe before yes notices,
		// or non-zero (SIGPIPE). Either way, check the output length.
		output := result.Output
		if err != nil {
			var he *HookError
			if errors.As(err, &he) {
				output = he.Output
			}
		}
		if len(output) != MaxHookOutputBytes {
			t.Errorf("output length = %d, want %d", len(output), MaxHookOutputBytes)
		}
	})

	t.Run("empty script rejected", func(t *testing.T) {
		t.Parallel()

		_, err := RunHook(context.Background(), HookParams{
			Script:    "",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})

		assertHookErrorOp(t, err, "validate")
	})

	t.Run("invalid dir rejected", func(t *testing.T) {
		t.Parallel()

		_, err := RunHook(context.Background(), HookParams{
			Script:    "echo hello",
			Dir:       "/nonexistent/path/that/does/not/exist",
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})

		assertHookErrorOp(t, err, "validate")
	})

	t.Run("non-positive timeout rejected", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			timeoutMS int
		}{
			{"zero", 0},
			{"negative", -1},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				_, err := RunHook(context.Background(), HookParams{
					Script:    "echo hello",
					Dir:       t.TempDir(),
					Env:       map[string]string{},
					TimeoutMS: tt.timeoutMS,
				})

				assertHookErrorOp(t, err, "validate")
			})
		}
	})

	t.Run("parent context cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel immediately so the hook never finishes.
		cancel()

		_, err := RunHook(ctx, HookParams{
			Script:    "sleep 60",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 30000,
		})

		assertHookErrorOp(t, err, "timeout")
	})

	t.Run("combined stdout and stderr captured", func(t *testing.T) {
		t.Parallel()

		result, err := RunHook(context.Background(), HookParams{
			Script:    "echo out; echo err >&2",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if !strings.Contains(result.Output, "out") {
			t.Errorf("Output = %q, want it to contain %q", result.Output, "out")
		}
		if !strings.Contains(result.Output, "err") {
			t.Errorf("Output = %q, want it to contain %q", result.Output, "err")
		}
	})

	// Section 5.3.4: SORTIE_ATTEMPT passed as string
	t.Run("SORTIE_ATTEMPT as string", func(t *testing.T) {
		t.Parallel()

		result, err := RunHook(context.Background(), HookParams{
			Script:    "echo $SORTIE_ATTEMPT",
			Dir:       t.TempDir(),
			Env:       map[string]string{"SORTIE_ATTEMPT": "2"},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if !strings.Contains(result.Output, "2") {
			t.Errorf("Output = %q, want it to contain %q", result.Output, "2")
		}
	})
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()

	t.Run("write within limit", func(t *testing.T) {
		t.Parallel()

		lb := &limitedBuffer{max: 256}
		data := bytes.Repeat([]byte("x"), 100)

		n, err := lb.Write(data)
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != 100 {
			t.Errorf("Write() = %d, want 100", n)
		}
		if len(lb.String()) != 100 {
			t.Errorf("String() length = %d, want 100", len(lb.String()))
		}
	})

	t.Run("write exceeds limit", func(t *testing.T) {
		t.Parallel()

		lb := &limitedBuffer{max: 200}
		data := bytes.Repeat([]byte("x"), 300)

		n, err := lb.Write(data)
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != 300 {
			t.Errorf("Write() = %d, want 300 (original length)", n)
		}
		if len(lb.String()) != 200 {
			t.Errorf("String() length = %d, want 200", len(lb.String()))
		}
	})

	t.Run("multiple writes with truncation", func(t *testing.T) {
		t.Parallel()

		lb := &limitedBuffer{max: 200}

		n1, _ := lb.Write(bytes.Repeat([]byte("a"), 150))
		if n1 != 150 {
			t.Errorf("first Write() = %d, want 150", n1)
		}

		n2, _ := lb.Write(bytes.Repeat([]byte("b"), 150))
		if n2 != 150 {
			t.Errorf("second Write() = %d, want 150", n2)
		}

		if len(lb.String()) != 200 {
			t.Errorf("String() length = %d, want 200", len(lb.String()))
		}
		// First 150 bytes are 'a', next 50 are 'b'.
		want := strings.Repeat("a", 150) + strings.Repeat("b", 50)
		if lb.String() != want {
			t.Errorf("String() content mismatch")
		}
	})

	t.Run("write after limit reached", func(t *testing.T) {
		t.Parallel()

		lb := &limitedBuffer{max: 100}

		lb.Write(bytes.Repeat([]byte("x"), 100)) //nolint:errcheck // test setup
		snapshot := lb.String()

		n, err := lb.Write([]byte("more data"))
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != len("more data") {
			t.Errorf("Write() = %d, want %d", n, len("more data"))
		}
		if lb.String() != snapshot {
			t.Error("String() changed after writing past limit")
		}
	})
}

func TestTruncateScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "short string",
			input: "echo hello",
			want:  "echo hello",
		},
		{
			name:  "exactly 200 chars",
			input: strings.Repeat("x", 200),
			want:  strings.Repeat("x", 200),
		},
		{
			name:  "over 200 chars",
			input: strings.Repeat("x", 250),
			want:  strings.Repeat("x", 200) + "...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateScript(tt.input)
			if got != tt.want {
				t.Errorf("truncateScript() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHookError(t *testing.T) {
	t.Parallel()

	t.Run("Error format with exit code", func(t *testing.T) {
		t.Parallel()

		he := &HookError{
			Op:       "run",
			Script:   "exit 42",
			ExitCode: 42,
			Err:      errors.New("exit status 42"),
		}
		want := "hook run: exit_code=42: exit status 42"
		if he.Error() != want {
			t.Errorf("Error() = %q, want %q", he.Error(), want)
		}
	})

	t.Run("Error format without exit code", func(t *testing.T) {
		t.Parallel()

		he := &HookError{
			Op:       "timeout",
			Script:   "sleep 60",
			ExitCode: -1,
			Err:      errors.New("hook timed out after 100ms"),
		}
		want := "hook timeout: hook timed out after 100ms"
		if he.Error() != want {
			t.Errorf("Error() = %q, want %q", he.Error(), want)
		}
	})

	t.Run("Error format validate", func(t *testing.T) {
		t.Parallel()

		he := &HookError{
			Op:       "validate",
			ExitCode: -1,
			Err:      errors.New("script must not be empty"),
		}
		want := "hook validate: script must not be empty"
		if he.Error() != want {
			t.Errorf("Error() = %q, want %q", he.Error(), want)
		}
	})

	t.Run("Error format start", func(t *testing.T) {
		t.Parallel()

		he := &HookError{
			Op:       "start",
			Script:   "bad-command",
			ExitCode: -1,
			Err:      errors.New("exec: not found"),
		}
		want := "hook start: exec: not found"
		if he.Error() != want {
			t.Errorf("Error() = %q, want %q", he.Error(), want)
		}
	})

	t.Run("Unwrap returns inner error", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("underlying cause")
		he := &HookError{Op: "run", ExitCode: 1, Err: inner}

		if he.Unwrap() != inner {
			t.Error("Unwrap() did not return the inner error")
		}
	})

	t.Run("errors.As extraction from wrapped chain", func(t *testing.T) {
		t.Parallel()

		inner := &HookError{
			Op:       "run",
			Script:   "exit 1",
			ExitCode: 1,
			Output:   "some output",
			Err:      errors.New("exit status 1"),
		}
		wrapped := fmt.Errorf("hook failed: %w", inner)

		var he *HookError
		if !errors.As(wrapped, &he) {
			t.Fatal("errors.As failed to extract *HookError from wrapped error")
		}
		if he.Op != "run" {
			t.Errorf("Op = %q, want %q", he.Op, "run")
		}
		if he.ExitCode != 1 {
			t.Errorf("ExitCode = %d, want 1", he.ExitCode)
		}
		if he.Output != "some output" {
			t.Errorf("Output = %q, want %q", he.Output, "some output")
		}
	})
}
