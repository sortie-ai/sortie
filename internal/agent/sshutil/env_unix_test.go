//go:build unix

package sshutil

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// shellRunner names one shell binary this file exercises the guarded
// import step against, when that shell is present on PATH.
type shellRunner struct {
	name string
	path string
}

// availableShells returns every shell this file knows how to drive that
// is actually installed, skipping the test cleanly when neither is
// present.
func availableShells(t *testing.T) []shellRunner {
	t.Helper()
	var shells []shellRunner
	for _, name := range []string{"bash", "dash"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		shells = append(shells, shellRunner{name: name, path: path})
	}
	if len(shells) == 0 {
		t.Skip("neither bash nor dash found on PATH")
	}
	return shells
}

// carriedTestValue holds a single quote, a double quote, an attempted
// command substitution naming markerPath, a backtick, a backslash, a
// space, a tab, a newline, a semicolon, a pipe, a glob character, and a
// non-ASCII rune, so a single-quoted shell assignment that leaks any of
// them fails this test.
func carriedTestValue(markerPath string) string {
	return "'\"$(touch " + markerPath + ")`\\ \t\n;|*héllo"
}

// mustReadPreamble drains launch's standard input preamble into a byte
// slice, or returns nil when the launch carries none.
func mustReadPreamble(t *testing.T, launch SSHLaunch) []byte {
	t.Helper()
	r := launch.StdinReader()
	if r == nil {
		return nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("StdinReader: %v", err)
	}
	return data
}

// importStepFixture holds the paths and launch a real-shell import-step
// test drives.
type importStepFixture struct {
	dir          string
	envFile      string
	trailFile    string
	markerPath   string
	verifyScript string
	emptyFile    string
	value        string
	trailing     string
	launch       SSHLaunch
	finalElement string
}

// buildImportStepFixture builds a workspace, a leading ". <empty file>
// &&" fragment, and a verify script that captures the carried
// variable's value and the trailing bytes that follow the import step
// on standard input.
func buildImportStepFixture(t *testing.T, shell shellRunner) importStepFixture {
	t.Helper()

	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty.sh")
	if err := os.WriteFile(emptyFile, nil, 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
		t.Fatalf("WriteFile(empty.sh): %v", err)
	}
	markerPath := filepath.Join(dir, "MARKER")
	envFile := filepath.Join(dir, "env.out")
	trailFile := filepath.Join(dir, "trail.out")
	verifyScript := filepath.Join(dir, "verify.sh")

	value := carriedTestValue(markerPath)
	trailing := "trailing protocol bytes\nwith a newline\n"

	script := "printf '%s' \"$TESTVAR\" > '" + envFile + "'\n" +
		"cat > '" + trailFile + "'\n"
	if err := os.WriteFile(verifyScript, []byte(script), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
		t.Fatalf("WriteFile(verify.sh): %v", err)
	}

	remoteCommand := ". '" + emptyFile + "' && '" + shell.path + "' '" + verifyScript + "'"
	launch := BuildSSHLaunch("host", dir, remoteCommand, nil, SSHOptions{
		Env: []EnvVar{{Name: "TESTVAR", Value: value}},
	})

	return importStepFixture{
		dir:          dir,
		envFile:      envFile,
		trailFile:    trailFile,
		markerPath:   markerPath,
		verifyScript: verifyScript,
		emptyFile:    emptyFile,
		value:        value,
		trailing:     trailing,
		launch:       launch,
		finalElement: launch.Args[len(launch.Args)-1],
	}
}

// TestBuildSSHLaunch_RealShellImportStep runs BuildSSHLaunch's final
// element through sh -c under every installed shell this file knows
// how to drive, feeding the preamble followed by a trailing byte
// sequence holding metacharacters and a non-ASCII rune. The command
// placed after the leading ". <empty file> &&" observes the value byte
// for byte, no marker file appears, and the trailing bytes arrive
// intact.
func TestBuildSSHLaunch_RealShellImportStep(t *testing.T) {
	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			fx := buildImportStepFixture(t, shell)
			stdin := append(append([]byte{}, mustReadPreamble(t, fx.launch)...), []byte(fx.trailing)...)

			cmd := exec.Command(shell.path, "-c", fx.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
			cmd.Stdin = bytes.NewReader(stdin)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s -c <final element> failed: %v, stderr: %s", shell.name, err, stderr.String())
			}

			if _, err := os.Stat(fx.markerPath); err == nil {
				t.Errorf("%s: marker file exists, want the command substitution never to run", shell.name)
			}

			gotValue, err := os.ReadFile(fx.envFile)
			if err != nil {
				t.Fatalf("ReadFile(env.out): %v", err)
			}
			if string(gotValue) != fx.value {
				t.Errorf("%s: carried value = %q, want %q", shell.name, string(gotValue), fx.value)
			}

			gotTrailing, err := os.ReadFile(fx.trailFile)
			if err != nil {
				t.Fatalf("ReadFile(trail.out): %v", err)
			}
			if string(gotTrailing) != fx.trailing {
				t.Errorf("%s: trailing bytes = %q, want %q", shell.name, string(gotTrailing), fx.trailing)
			}
		})
	}
}

// TestBuildSSHLaunch_RealShellImportStep_PreambleWithheld runs the
// final element with the trailing bytes alone on standard input, no
// preamble ahead of them. The import step then evaluates a deliberately
// malformed fragment cut from those bytes, which fails every shell this
// file drives, so the verify script never runs.
func TestBuildSSHLaunch_RealShellImportStep_PreambleWithheld(t *testing.T) {
	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			fx := buildImportStepFixture(t, shell)
			const malformed = `"unterminated stdin bytes with no closing quote`

			cmd := exec.Command(shell.path, "-c", fx.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
			cmd.Stdin = bytes.NewReader([]byte(malformed))
			if err := cmd.Run(); err == nil {
				t.Errorf("%s: command succeeded with the preamble withheld, want a failure before the verify script ran", shell.name)
			}

			if _, err := os.Stat(fx.envFile); err == nil {
				t.Errorf("%s: verify script ran with the preamble withheld, want it never to run", shell.name)
			}
		})
	}
}

// TestBuildSSHLaunch_RealShellImportStep_ShortRead runs the final
// element with the exact preamble bytes minus its last byte, which
// removes the closing quote of the carried value's shell-quoted
// assignment and produces a syntax error every shell this file drives
// rejects before the verify script runs.
func TestBuildSSHLaunch_RealShellImportStep_ShortRead(t *testing.T) {
	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			fx := buildImportStepFixture(t, shell)
			preamble := mustReadPreamble(t, fx.launch)
			short := preamble[:len(preamble)-1]

			cmd := exec.Command(shell.path, "-c", fx.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
			cmd.Stdin = bytes.NewReader(short)
			if err := cmd.Run(); err == nil {
				t.Errorf("%s: command succeeded on a one-byte-short read, want a syntax error before the verify script ran", shell.name)
			}

			if _, err := os.Stat(fx.envFile); err == nil {
				t.Errorf("%s: verify script ran on a one-byte-short read, want it never to run", shell.name)
			}
		})
	}
}

// TestBuildSSHLaunch_RealShellImportStep_NoDD runs the final element
// with a PATH that resolves no dd binary. The guard exits 1 before the
// verify script runs, and standard error is exactly the guard's
// message line.
func TestBuildSSHLaunch_RealShellImportStep_NoDD(t *testing.T) {
	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			fx := buildImportStepFixture(t, shell)
			stdin := append(append([]byte{}, mustReadPreamble(t, fx.launch)...), []byte(fx.trailing)...)
			noDDDir := t.TempDir()

			cmd := exec.Command(shell.path, "-c", fx.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
			cmd.Env = []string{"PATH=" + noDDDir}
			cmd.Stdin = bytes.NewReader(stdin)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("%s: command succeeded with no dd on PATH, want exit 1", shell.name)
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%s: error = %v (%T), want *exec.ExitError", shell.name, err, err)
			}
			if exitErr.ExitCode() != 1 {
				t.Errorf("%s: exit code = %d, want 1", shell.name, exitErr.ExitCode())
			}
			if got := stderr.String(); got != ddMissingMessage+"\n" {
				t.Errorf("%s: stderr = %q, want exactly %q", shell.name, got, ddMissingMessage+"\n")
			}
			if _, statErr := os.Stat(fx.envFile); statErr == nil {
				t.Errorf("%s: verify script ran with no dd on PATH, want it never to run", shell.name)
			}
		})
	}
}
