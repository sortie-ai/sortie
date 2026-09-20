package agenttest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Scenario is the body of a fake runtime. It runs inside a re-executed copy of
// the test binary, receives the launch arguments and the [FakeRuntime]
// parameters, and returns the process exit code.
type Scenario func(args []string, params json.RawMessage) int

// OutputScenario names the built-in [Scenario] every package can launch without
// registering it. Its parameters are an [Output].
const OutputScenario = "agenttest.output"

// Output parameterizes [OutputScenario]: the runtime writes Stdout, then
// Stderr, and exits with ExitCode, or stays alive until killed when Hang is set.
type Output struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Hang     bool
}

type fakeConfig struct {
	Scenario string
	Params   json.RawMessage
}

// staged is the copy of the test binary every fake runtime links to,
// created by [Main] before the package's tests run.
var staged string

// Main is the whole TestMain body of a package whose tests call [FakeRuntime].
// A process started from a fake runtime executable runs its scenario from
// scenarios and exits; any other process runs the package's tests.
func Main(m *testing.M, scenarios map[string]Scenario) {
	exe, err := os.Executable()
	if err == nil {
		if config, readErr := os.ReadFile(configPath(exe)); readErr == nil {
			code := runScenario(config, scenarios)
			// The scenario has returned, so nothing more can spawn and the
			// process is still around to be asked what it left running.
			recordDescendants(exe)
			os.Exit(code)
		}
	}

	// Fake runtimes link to this staged copy rather than to the test binary:
	// Windows refuses to delete any name of a running image, so a link to the
	// test binary would survive t.TempDir cleanup. Copying before m.Run starts
	// a goroutine that can fork also keeps the copy clear of the ETXTBSY race
	// (golang/go#22315).
	var stagedDir string
	if err == nil {
		if dir, mkErr := os.MkdirTemp("", "agenttest-fakeruntime"); mkErr == nil {
			stagedDir = dir
			staged = filepath.Join(dir, "runtime")
			if runtime.GOOS == "windows" {
				staged += ".exe"
			}
			if copyErr := copyExecutable(exe, staged); copyErr != nil {
				staged = ""
			}
		}
	}

	code := m.Run()
	if stagedDir != "" {
		_ = os.RemoveAll(stagedDir)
	}
	os.Exit(code)
}

// Typed adapts run into a [Scenario] whose parameters decode into P.
func Typed[P any](run func(args []string, params P) int) Scenario {
	return func(args []string, raw json.RawMessage) int {
		var params P
		if err := json.Unmarshal(raw, &params); err != nil {
			fmt.Fprintf(os.Stderr, "fake runtime: decode params: %v\n", err)
			return 2
		}
		return run(args, params)
	}
}

// Run writes Stdout and Stderr, blocks when Hang is set, and reports the exit
// status. A failed write ends the runtime with status 2 instead of ExitCode, so
// a fixture never reports the success of output the test never received.
func (o Output) Run() int {
	if _, err := io.WriteString(os.Stdout, o.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fake runtime: write stdout: %v\n", err)
		return 2
	}
	if _, err := io.WriteString(os.Stderr, o.Stderr); err != nil {
		return 2
	}
	if o.Hang {
		Hang()
	}
	return o.ExitCode
}

// Hang blocks until the process is killed. It sleeps rather than blocking on a
// channel because a pending timer keeps the runtime's deadlock detector from
// ending the process on its own.
func Hang() {
	for {
		time.Sleep(time.Hour)
	}
}

// FakeRuntime creates an executable named name in dir that runs scenario with
// params, and returns its path (with a .exe suffix on Windows). The executable
// is the test binary itself under a new name, so the calling package's TestMain
// must call [Main].
func FakeRuntime(t testing.TB, dir, name, scenario string, params any) string {
	t.Helper()

	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("FakeRuntime: encode params: %v", err)
	}
	config, err := json.Marshal(fakeConfig{Scenario: scenario, Params: encoded})
	if err != nil {
		t.Fatalf("FakeRuntime: encode config: %v", err)
	}

	if staged == "" {
		t.Fatal("FakeRuntime: TestMain must call agenttest.Main")
	}
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if err := os.WriteFile(configPath(path), config, 0o600); err != nil {
		t.Fatalf("FakeRuntime: %v", err)
	}

	// A hard link never opens the executable for writing, so it avoids the
	// ETXTBSY race a freshly written executable meets when another goroutine
	// forks (golang/go#22315). The copy covers a staged binary on another file
	// system than dir.
	if err := os.Link(staged, path); err != nil {
		if err := copyExecutable(staged, path); err != nil {
			t.Fatalf("FakeRuntime: %v", err)
		}
	}

	// Windows holds a process image briefly after exit, so t.TempDir cleanup
	// can meet "Access is denied" on this file. Cleanups run in reverse order,
	// so this one runs before the directory is removed and can wait the handle
	// out.
	t.Cleanup(func() {
		removeEventually(t, path)
		removeEventually(t, configPath(path))
	})

	return path
}

// removeEventually deletes path, retrying while the file system says it is still
// in use. It never fails the test: turning a slow handle into a hard failure
// here would trade one flake for another.
func removeEventually(t testing.TB, path string) {
	t.Helper()

	const (
		budget   = 2 * time.Second
		interval = 20 * time.Millisecond
	)

	deadline := time.Now().Add(budget)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Logf("FakeRuntime: could not remove %s after %s: %v", path, budget, err)
			return
		}
		time.Sleep(interval)
	}
}

func runScenario(config []byte, scenarios map[string]Scenario) int {
	var cfg fakeConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "fake runtime: decode config: %v\n", err)
		return 2
	}
	run := scenarios[cfg.Scenario]
	if cfg.Scenario == OutputScenario {
		run = Typed(writeOutput)
	}
	if run == nil {
		fmt.Fprintf(os.Stderr, "fake runtime: unknown scenario %q\n", cfg.Scenario)
		return 2
	}
	return run(os.Args[1:], cfg.Params)
}

func writeOutput(_ []string, out Output) int {
	return out.Run()
}

func configPath(exe string) string {
	return strings.TrimSuffix(exe, ".exe") + ".fake.json"
}

// DescendantReceipt names the file the program at commandPath appends its own
// live children's process ids to as it exits, so a launcher can charge itself
// with whatever that program left running. The program's own record catches a
// child born between two samples of the process table, which sampling from
// outside would miss.
func DescendantReceipt(commandPath string) string {
	return strings.TrimSuffix(commandPath, ".exe") + ".descendants"
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the running test binary
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // dst is under the caller's test directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
