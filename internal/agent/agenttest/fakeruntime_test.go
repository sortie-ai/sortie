package agenttest_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// scenarios is filled by this package's test files before TestMain runs, so a
// scenario only one platform can carry stays in the file that platform builds.
var scenarios = map[string]agenttest.Scenario{
	"echo-args": agenttest.Typed(func(args []string, prefix string) int {
		fmt.Print(prefix + strings.Join(args, " "))
		return 3
	}),
}

func TestMain(m *testing.M) {
	agenttest.Main(m, scenarios)
}

func TestFakeRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scenario   string
		params     any
		args       []string
		wantStdout string
		wantStderr string
		wantCode   int
	}{
		{
			name:       "built-in output",
			scenario:   agenttest.OutputScenario,
			params:     agenttest.Output{Stdout: "out\n", Stderr: "err\n", ExitCode: 7},
			wantStdout: "out\n",
			wantStderr: "err\n",
			wantCode:   7,
		},
		{
			name:       "package scenario receives args and params",
			scenario:   "echo-args",
			params:     "args: ",
			args:       []string{"a", "b"},
			wantStdout: "args: a b",
			wantCode:   3,
		},
		{
			name:       "unknown scenario",
			scenario:   "missing",
			wantStderr: "fake runtime: unknown scenario \"missing\"\n",
			wantCode:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := agenttest.FakeRuntime(t, t.TempDir(), "runtime", tt.scenario, tt.params)
			cmd := exec.Command(path, tt.args...) //nolint:gosec // path is a fake runtime under t.TempDir()
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			code := 0
			var exitErr *exec.ExitError
			if err := cmd.Run(); errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func TestFakeRuntime_WhenArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		params     agenttest.Output
		args       []string
		wantStdout string
		wantStderr string
		wantCode   int
	}{
		{
			name:       "launch carrying the named argument replays the configured output",
			params:     agenttest.Output{Stdout: "out\n", Stderr: "err\n", ExitCode: 7, WhenArg: "--sortie-unknown-switch"},
			args:       []string{"--sortie-unknown-switch"},
			wantStdout: "out\n",
			wantStderr: "err\n",
			wantCode:   7,
		},
		{
			name:   "launch not carrying the named argument writes nothing and exits 0",
			params: agenttest.Output{Stdout: "out\n", Stderr: "err\n", ExitCode: 7, WhenArg: "--sortie-unknown-switch"},
			args:   []string{"--version"},
		},
		{
			name:       "empty WhenArg keeps the unconditional behavior",
			params:     agenttest.Output{Stdout: "out\n", Stderr: "err\n", ExitCode: 7},
			args:       []string{"--version"},
			wantStdout: "out\n",
			wantStderr: "err\n",
			wantCode:   7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, tt.params)
			cmd := exec.Command(path, tt.args...) //nolint:gosec // path is a fake runtime under t.TempDir()
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			code := 0
			var exitErr *exec.ExitError
			if err := cmd.Run(); errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func TestFakeRuntime_Hang(t *testing.T) {
	t.Parallel()

	path := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{Hang: true})
	cmd := exec.Command(path) //nolint:gosec // path is a fake runtime under t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		t.Fatalf("hanging runtime exited: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	<-done
}

func TestFakeRuntimeRemovesItsBinaryBeforeTheDirectoryGoes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var exe string

	t.Run("inner", func(t *testing.T) {
		exe = agenttest.FakeRuntime(t, dir, "agent", agenttest.OutputScenario, agenttest.Output{})
		if _, err := os.Stat(exe); err != nil {
			t.Fatalf("fake runtime was not created: %v", err)
		}
	})

	if _, err := os.Stat(exe); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fake runtime still present after cleanup: %v", err)
	}
	config := strings.TrimSuffix(exe, ".exe") + ".fake.json"
	if _, err := os.Stat(config); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fake runtime config still present after cleanup: %v", err)
	}
}

func TestFakeRuntimeCleanupSurvivesAnUnremovableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	t.Run("inner", func(t *testing.T) {
		exe := agenttest.FakeRuntime(t, dir, "agent", agenttest.OutputScenario, agenttest.Output{})
		// Remove it early: the cleanup then finds nothing, which is the same
		// path a file that vanished underneath it takes.
		if err := os.Remove(exe); err != nil {
			t.Fatalf("remove: %v", err)
		}
	})
}
