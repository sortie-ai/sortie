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

// TestBuildSSHLaunch_RealShellImportStep_ValidPrefixShortRead runs the
// final element with the preamble truncated to its leading unset
// command, a prefix that is valid shell on its own and evaluates
// without error. dd reports success when its input ends before the
// count it was given, so the completion marker is the only thing that
// separates this from a whole preamble: the import step fails and the
// verify script never runs, rather than the agent starting with none
// of the variables the launch was supposed to carry.
func TestBuildSSHLaunch_RealShellImportStep_ValidPrefixShortRead(t *testing.T) {
	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			fx := buildImportStepFixture(t, shell)
			preamble := mustReadPreamble(t, fx.launch)
			cut := bytes.Index(preamble, []byte(" && export "))
			if cut <= 0 {
				t.Fatalf("test fixture error: preamble %q carries no export clause", string(preamble))
			}

			cmd := exec.Command(shell.path, "-c", fx.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
			cmd.Stdin = bytes.NewReader(preamble[:cut])
			if err := cmd.Run(); err == nil {
				t.Errorf("%s: command succeeded on a valid-prefix short read, want a failure before the verify script ran", shell.name)
			}

			if _, err := os.Stat(fx.envFile); err == nil {
				t.Errorf("%s: verify script ran on a valid-prefix short read, want it never to run", shell.name)
			}
		})
	}
}

// bypassFixture holds a launch whose remote command carries a
// top-level shell operator, plus the marker each side of that operator
// touches, so a test can tell which of them ran.
type bypassFixture struct {
	launch       SSHLaunch
	finalElement string
	agentMarker  string
	tailMarker   string
}

// buildBypassFixture builds a launch whose remote command is
// "touch <agentMarker> <operator> touch <tailMarker>", so each marker
// records whether its own side of the operator ran.
func buildBypassFixture(t *testing.T, operator string) bypassFixture {
	t.Helper()

	dir := t.TempDir()
	agentMarker := filepath.Join(dir, "AGENT")
	tailMarker := filepath.Join(dir, "TAIL")

	remoteCommand := "touch '" + agentMarker + "' " + operator + " touch '" + tailMarker + "'"
	launch := BuildSSHLaunch("host", dir, remoteCommand, nil, SSHOptions{
		Env: []EnvVar{{Name: "TESTVAR", Value: "carried"}},
	})

	return bypassFixture{
		launch:       launch,
		finalElement: launch.Args[len(launch.Args)-1],
		agentMarker:  agentMarker,
		tailMarker:   tailMarker,
	}
}

// TestBuildSSHLaunch_RealShellImportStep_NoBypassByCommandOperator runs
// a final element whose remote command carries a top-level || or ;
// under every installed shell this file drives. With the preamble
// delivered both the command and its operator behave normally; with the
// preamble withheld the import step fails and neither side runs. An
// ungrouped fragment would bind that operator to the launch's own &&
// chain, so its right-hand side would start the agent carrying none of
// the variables the launch was to deliver.
func TestBuildSSHLaunch_RealShellImportStep_NoBypassByCommandOperator(t *testing.T) {
	for _, shell := range availableShells(t) {
		for _, operator := range []string{"||", ";"} {
			t.Run(shell.name+"/"+operator, func(t *testing.T) {
				t.Parallel()

				delivered := buildBypassFixture(t, operator)
				stdin := append(append([]byte{}, mustReadPreamble(t, delivered.launch)...), []byte("\n")...)
				cmd := exec.Command(shell.path, "-c", delivered.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
				cmd.Stdin = bytes.NewReader(stdin)
				if err := cmd.Run(); err != nil {
					t.Fatalf("%s: command failed with the preamble delivered: %v", shell.name, err)
				}
				if _, err := os.Stat(delivered.agentMarker); err != nil {
					t.Fatalf("%s: agent side did not run with the preamble delivered, so this fixture cannot observe a bypass", shell.name)
				}

				withheld := buildBypassFixture(t, operator)
				const malformed = `"unterminated stdin bytes with no closing quote`
				cmd = exec.Command(shell.path, "-c", withheld.finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
				cmd.Stdin = bytes.NewReader([]byte(malformed))
				if err := cmd.Run(); err == nil {
					t.Errorf("%s: command succeeded with the preamble withheld, want a failure", shell.name)
				}
				if _, err := os.Stat(withheld.agentMarker); err == nil {
					t.Errorf("%s: left side of %q ran with the preamble withheld, want neither side to run", shell.name, operator)
				}
				if _, err := os.Stat(withheld.tailMarker); err == nil {
					t.Errorf("%s: right side of %q ran with the preamble withheld, want neither side to run", shell.name, operator)
				}
			})
		}
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

// TestBuildSSHLaunch_RealShellAgentCommandTerminator runs a final
// element whose remote command ends in a top-level ; or &, with a
// plain command and one carrying an agent argument as controls, under
// every installed shell this file drives. An operator's own command
// reaches the remote shell unsplit, so a group a semicolon closes turns
// either ending into a syntax error the shell rejects before the agent
// runs.
func TestBuildSSHLaunch_RealShellAgentCommandTerminator(t *testing.T) {
	tests := []struct {
		name string
		// suffix ends the operator's own command, reaching the remote
		// shell unsplit as agent.command does.
		suffix string
		// withArg appends an agent argument the launch shell-quotes.
		withArg bool
		// background ends the command with &, so the group returns
		// before the command has necessarily touched its marker.
		background bool
	}{
		{name: "plain"},
		{name: "with agent argument", withArg: true},
		{name: "trailing semicolon", suffix: ";"},
		{name: "trailing ampersand", suffix: " &", background: true},
	}

	for _, shell := range availableShells(t) {
		for _, tt := range tests {
			t.Run(shell.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				dir := t.TempDir()
				agentMarker := filepath.Join(dir, "AGENT")
				argMarker := filepath.Join(dir, "ARG")

				var agentArgs []string
				if tt.withArg {
					agentArgs = []string{argMarker}
				}
				launch := BuildSSHLaunch("host", dir, "touch '"+agentMarker+"'"+tt.suffix, agentArgs, SSHOptions{
					Env: []EnvVar{{Name: "TESTVAR", Value: "carried"}},
				})
				finalElement := launch.Args[len(launch.Args)-1]

				cmd := exec.Command(shell.path, "-c", finalElement) //nolint:gosec // shell.path resolved via exec.LookPath, finalElement built from t.TempDir() paths
				cmd.Stdin = bytes.NewReader(mustReadPreamble(t, launch))
				var stderr bytes.Buffer
				cmd.Stderr = &stderr

				if err := cmd.Run(); err != nil {
					t.Fatalf("%s: %q failed: %v, stderr: %s", shell.name, finalElement, err, stderr.String())
				}
				if got := stderr.String(); got != "" {
					t.Errorf("%s: stderr = %q, want empty", shell.name, got)
				}

				if tt.background {
					return
				}
				if _, err := os.Stat(agentMarker); err != nil {
					t.Errorf("%s: agent command did not run: %v", shell.name, err)
				}
				if tt.withArg {
					if _, err := os.Stat(argMarker); err != nil {
						t.Errorf("%s: agent argument did not reach the command: %v", shell.name, err)
					}
				}
			})
		}
	}
}
