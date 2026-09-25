//go:build unix

package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/redact"
)

// hookLogBuffer is a bytes.Buffer guarded by a mutex, for capturing
// records logged through slog.Default while a hook's own subprocess
// output is drained concurrently.
type hookLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *hookLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *hookLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

// truncationMarker returns the prefix [formatHookOutput] adds once the
// underlying [procutil.TailBuffer] has discarded earlier output to stay
// within [MaxHookOutputBytes].
func truncationMarker() string {
	return fmt.Sprintf("[truncated: showing last %d bytes of hook output]\n", MaxHookOutputBytes)
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

		// EvalSymlinks handles /tmp -> /private/tmp on macOS.
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

	t.Run("workspace path is a symbolic link", func(t *testing.T) {
		t.Parallel()

		target := t.TempDir()
		marker := filepath.Join(target, "marker")
		link := filepath.Join(t.TempDir(), "workspace-link")
		mustSymlink(t, target, link)

		_, err := RunHook(context.Background(), HookParams{
			Script:    "touch " + marker,
			Dir:       link,
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})

		assertHookErrorOp(t, err, "validate")
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Errorf("marker file %q exists, want the hook process never to start", marker)
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

	// Process group kill prevents orphaned grandchildren.
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
		// ESRCH means the process group no longer exists, which is what we want.
		if err := syscall.Kill(-pgid, 0); err == nil {
			t.Error("process group still alive after timeout; expected it to be killed")
		}
	})

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
		// or non-zero (SIGPIPE). Either way, check the captured output.
		output := result.Output
		if err != nil {
			if he, ok := errors.AsType[*HookError](err); ok {
				output = he.Output
			}
		}

		marker := truncationMarker()
		if !strings.HasPrefix(output, marker) {
			t.Errorf("Output prefix = %q, want %q", output[:min(len(output), len(marker))], marker)
		}
		if maxLen := len(marker) + MaxHookOutputBytes; len(output) > maxLen {
			t.Errorf("len(Output) = %d, want <= %d (MaxHookOutputBytes + marker)", len(output), maxLen)
		}
	})

	t.Run("verbose hook keeps tail with final error line, not head", func(t *testing.T) {
		t.Parallel()

		const finalLine = "DISTINCT_FINAL_ERROR_LINE_42"
		script := `yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA | head -c 307200; echo ` + finalLine + `; exit 1`

		_, err := RunHook(context.Background(), HookParams{
			Script:    script,
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 10000,
		})

		he := requireHookError(t, err)
		if !strings.Contains(he.Output, finalLine) {
			tail := he.Output
			if len(tail) > 80 {
				tail = tail[len(tail)-80:]
			}
			t.Errorf("Output tail = %q, want it to contain %q", tail, finalLine)
		}
		marker := truncationMarker()
		if !strings.HasPrefix(he.Output, marker) {
			t.Errorf("Output prefix = %q, want %q", he.Output[:min(len(he.Output), len(marker))], marker)
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

// TestRunHook_RestrictedEnv verifies that the hook environment is restricted
// to prevent secret leakage from the host process environment.
func TestRunHook_RestrictedEnv(t *testing.T) {
	t.Run("excludes unrelated variables", func(t *testing.T) {
		t.Setenv("TEST_SECRET_LEAK_CANARY", "s3cret_value_42")

		result, err := RunHook(context.Background(), HookParams{
			Script:    "env",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if strings.Contains(result.Output, "TEST_SECRET_LEAK_CANARY") {
			t.Error("hook env contains TEST_SECRET_LEAK_CANARY; want it excluded")
		}
		wantPath := os.Getenv("PATH")
		if wantPath == "" {
			t.Skip("PATH not set in parent environment")
		}
		if !strings.Contains(result.Output, "PATH="+wantPath) {
			t.Errorf("hook env missing PATH=%s; want it inherited with exact value", wantPath)
		}
	})

	t.Run("includes SORTIE_ from parent", func(t *testing.T) {
		t.Setenv("SORTIE_CUSTOM_VAR", "fromparent")

		result, err := RunHook(context.Background(), HookParams{
			Script:    "echo $SORTIE_CUSTOM_VAR",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if !strings.Contains(result.Output, "fromparent") {
			t.Errorf("Output = %q, want it to contain %q", result.Output, "fromparent")
		}
	})

	t.Run("params.Env overrides SORTIE_ from parent", func(t *testing.T) {
		t.Setenv("SORTIE_ISSUE_ID", "parent_value")

		result, err := RunHook(context.Background(), HookParams{
			Script:    "echo $SORTIE_ISSUE_ID",
			Dir:       t.TempDir(),
			Env:       map[string]string{"SORTIE_ISSUE_ID": "override_value"},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if !strings.Contains(result.Output, "override_value") {
			t.Errorf("Output = %q, want it to contain %q", result.Output, "override_value")
		}
		if strings.Contains(result.Output, "parent_value") {
			t.Error("Output contains parent_value; want override to win")
		}
	})

	t.Run("HOME and SHELL are inherited", func(t *testing.T) {
		wantHome := os.Getenv("HOME")
		wantShell := os.Getenv("SHELL")
		if wantHome == "" || wantShell == "" {
			t.Skip("HOME or SHELL not set in parent environment")
		}

		result, err := RunHook(context.Background(), HookParams{
			Script:    "echo HOME=$HOME; echo SHELL=$SHELL",
			Dir:       t.TempDir(),
			Env:       map[string]string{},
			TimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("RunHook() error: %v", err)
		}
		if !strings.Contains(result.Output, "HOME="+wantHome) {
			t.Errorf("Output = %q, want it to contain HOME=%s", result.Output, wantHome)
		}
		if !strings.Contains(result.Output, "SHELL="+wantShell) {
			t.Errorf("Output = %q, want it to contain SHELL=%s", result.Output, wantShell)
		}
	})
}

func TestFormatHookOutput(t *testing.T) {
	t.Parallel()

	t.Run("write within limit", func(t *testing.T) {
		t.Parallel()

		buf := procutil.NewTailBuffer(256)
		data := bytes.Repeat([]byte("x"), 100)

		n, err := buf.Write(data)
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != 100 {
			t.Errorf("Write() = %d, want 100", n)
		}
		if got := formatHookOutput(buf); len(got) != 100 {
			t.Errorf("formatHookOutput() length = %d, want 100", len(got))
		}
	})

	t.Run("write exceeds limit", func(t *testing.T) {
		t.Parallel()

		buf := procutil.NewTailBuffer(MaxHookOutputBytes)
		data := bytes.Repeat([]byte("x"), MaxHookOutputBytes+100)

		n, err := buf.Write(data)
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != len(data) {
			t.Errorf("Write() = %d, want %d (original length)", n, len(data))
		}

		got := formatHookOutput(buf)
		marker := truncationMarker()
		if !strings.HasPrefix(got, marker) {
			t.Errorf("formatHookOutput() = %.80q..., want prefix %q", got, marker)
		}
		tail := strings.TrimPrefix(got, marker)
		if want := string(data[len(data)-MaxHookOutputBytes:]); tail != want {
			t.Errorf("formatHookOutput() tail length = %d, want %d matching the last %d bytes written", len(tail), len(want), MaxHookOutputBytes)
		}
	})

	t.Run("multiple writes with truncation", func(t *testing.T) {
		t.Parallel()

		buf := procutil.NewTailBuffer(MaxHookOutputBytes)

		n1, _ := buf.Write(bytes.Repeat([]byte("a"), MaxHookOutputBytes))
		if n1 != MaxHookOutputBytes {
			t.Errorf("first Write() = %d, want %d", n1, MaxHookOutputBytes)
		}

		n2, _ := buf.Write(bytes.Repeat([]byte("b"), 150))
		if n2 != 150 {
			t.Errorf("second Write() = %d, want 150", n2)
		}

		// Retained tail is the last MaxHookOutputBytes bytes: the first
		// write's 'a's, minus the 150 bytes the second write's 'b's
		// pushed out, followed by all 150 'b's.
		marker := truncationMarker()
		want := marker + strings.Repeat("a", MaxHookOutputBytes-150) + strings.Repeat("b", 150)
		if got := formatHookOutput(buf); got != want {
			t.Errorf("formatHookOutput() length = %d, want %d matching the expected tail window", len(got), len(want))
		}
	})

	t.Run("write after limit reached", func(t *testing.T) {
		t.Parallel()

		buf := procutil.NewTailBuffer(MaxHookOutputBytes)

		buf.Write(bytes.Repeat([]byte("x"), MaxHookOutputBytes)) //nolint:errcheck // test setup
		snapshot := formatHookOutput(buf)

		n, err := buf.Write([]byte("more data"))
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		if n != len("more data") {
			t.Errorf("Write() = %d, want %d", n, len("more data"))
		}

		// Tail-retention shifts the window: the write past the limit
		// discards the earliest bytes, so the formatted output now
		// differs from the pre-write snapshot and ends with what was
		// just written.
		got := formatHookOutput(buf)
		if got == snapshot {
			t.Error("formatHookOutput() unchanged after writing past limit, want it to reflect the shifted tail")
		}
		if !strings.HasSuffix(got, "more data") {
			t.Errorf("formatHookOutput() = %.80q..., want suffix %q", got, "more data")
		}
	})

	t.Run("no truncation is byte-identical with no prefix", func(t *testing.T) {
		t.Parallel()

		buf := procutil.NewTailBuffer(MaxHookOutputBytes)
		buf.Write([]byte("plain hook output, no secrets")) //nolint:errcheck // test setup

		if got, want := formatHookOutput(buf), "plain hook output, no secrets"; got != want {
			t.Errorf("formatHookOutput() = %q, want %q (byte-identical, no truncation marker)", got, want)
		}
	})

	t.Run("registered value masked", func(t *testing.T) {
		t.Parallel()

		value := "hook-secret-" + randomHookTestSuffix(t)
		redact.Add("test.hook registered value", value)

		buf := procutil.NewTailBuffer(MaxHookOutputBytes)
		buf.Write([]byte("script printed " + value)) //nolint:errcheck // test setup

		got := formatHookOutput(buf)
		if strings.Contains(got, value) {
			t.Fatalf("formatHookOutput() = %q, leaked the registered value", got)
		}
		if !strings.Contains(got, redact.Marker) {
			t.Errorf("formatHookOutput() = %q, want it to contain %q", got, redact.Marker)
		}
	})

	t.Run("registered value straddling the retention cut is masked", func(t *testing.T) {
		t.Parallel()

		// The retention cut lands inside the value's raw byte span only
		// when the value itself is longer than MaxHookOutputBytes, so the
		// filler tail makes the value straddle the boundary rather than
		// fitting entirely inside the kept tail.
		value := "hook-straddle-" + randomHookTestSuffix(t) + strings.Repeat("V", MaxHookOutputBytes)
		redact.Add("test.hook straddling value", value)

		buf := procutil.NewTailBuffer(MaxHookOutputBytes)
		buf.Write([]byte("prefix-")) //nolint:errcheck // test setup
		buf.Write([]byte(value))     //nolint:errcheck // test setup

		got := formatHookOutput(buf)
		if !strings.Contains(got, redact.Marker) {
			t.Fatalf("formatHookOutput() = %q, want it to contain %q", got, redact.Marker)
		}
		if strings.Contains(got, value) {
			t.Fatalf("formatHookOutput() = %q, leaked the full value straddling the retention cut", got)
		}
		if tail := value[len(value)-64:]; strings.Contains(got, tail) {
			t.Fatalf("formatHookOutput() = %q, leaked a byte run %q from the value's tail", got, tail)
		}
	})
}

func randomHookTestSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(buf)
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

// TestRunHook_HeldAndNullRedirectedBackgroundProcessesTerminated covers
// a script that prints a line, backgrounds a held descendant
// holding the hook's output, and backgrounds a second process whose
// output is redirected away, then exits 0. RunHook returns within 2s
// with the line and no error, logs no CaptureAbandonedWarning record,
// and both background processes are gone.
func TestRunHook_HeldAndNullRedirectedBackgroundProcessesTerminated(t *testing.T) {
	origDefault := slog.Default()
	logBuf := &hookLogBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(origDefault) })

	dir := t.TempDir()
	heldPIDFile := filepath.Join(dir, "held.pid")
	nullPIDFile := filepath.Join(dir, "null.pid")
	script := fmt.Sprintf(
		"echo hello\nsleep 30 & echo $! > %q\nsleep 30 >/dev/null 2>&1 & echo $! > %q\n",
		heldPIDFile, nullPIDFile,
	)

	start := time.Now()
	result, err := RunHook(context.Background(), HookParams{
		Script:    script,
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("RunHook() took %v, want within 2s", elapsed)
	}
	if err != nil {
		t.Fatalf("RunHook() error = %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "hello")
	}
	if out := logBuf.String(); strings.Contains(out, procutil.CaptureAbandonedWarning) {
		t.Errorf("log output contains %q, want no abandonment WARN; got %q", procutil.CaptureAbandonedWarning, out)
	}

	heldPID := readHookPIDFile(t, heldPIDFile)
	nullPID := readHookPIDFile(t, nullPIDFile)
	assertHookProcessGone(t, heldPID)
	assertHookProcessGone(t, nullPID)
}

// readHookPIDFile reads a PID a background job wrote via "echo $!" to
// path.
func readHookPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v", path, err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
		t.Fatalf("parse PID from %q = %q: %v", path, data, err)
	}
	return pid
}

// assertHookProcessGone polls until kill(pid, 0) reports ESRCH, or
// fails t after a bound well under the 30s the fixture's background
// sleeps run for.
func assertHookProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d still answers signal 0, want it gone", pid)
}

// TestRunHook_XDGRuntimeDirAndDBusAddressInherited pins that a hook
// receives XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS from the
// parent process with their exact values, so it can reach a systemd
// user manager, while an unrelated variable stays excluded.
func TestRunHook_XDGRuntimeDirAndDBusAddressInherited(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1234/bus")
	t.Setenv("TEST_SECRET_LEAK_CANARY", "leak-me-not")

	result, err := RunHook(context.Background(), HookParams{
		Script:    "env",
		Dir:       t.TempDir(),
		Env:       map[string]string{},
		TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("RunHook() error: %v", err)
	}
	if !strings.Contains(result.Output, "XDG_RUNTIME_DIR=/run/user/1234") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "XDG_RUNTIME_DIR=/run/user/1234")
	}
	if !strings.Contains(result.Output, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1234/bus") {
		t.Errorf("Output = %q, want it to contain %q", result.Output, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1234/bus")
	}
	if strings.Contains(result.Output, "TEST_SECRET_LEAK_CANARY") {
		t.Error("hook env contains TEST_SECRET_LEAK_CANARY, want it excluded")
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
