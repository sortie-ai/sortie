//go:build unix

package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

const scenarioOpencodeSSHStandIn = "opencode.ssh-stand-in"

func init() {
	fakeScenarios[scenarioOpencodeSSHStandIn] = agenttest.Typed(runOpencodeSSHStandIn)
}

type sshStandInParams struct {
	PATH string
}

func runOpencodeSSHStandIn(args []string, params sshStandInParams) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ssh stand-in: no arguments")
		return 2
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh stand-in: sh not found: %v\n", err)
		return 2
	}

	// The dropped environment proves carried values reach the remote command
	// only through standard input.
	remoteCommand := args[len(args)-1]
	cmd := exec.Command(shPath, "-c", remoteCommand) //nolint:gosec // remoteCommand is the launch this test built
	cmd.Env = []string{"PATH=" + params.PATH}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "ssh stand-in: %v\n", err)
		return 2
	}
	return 0
}

func sshStandInEnvPath(t *testing.T, includeDD bool) string {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH: %v", err)
	}

	// A private directory: sh and dd usually share /usr/bin, so reusing sh's
	// own directory would resolve dd in the no-dd case too.
	dir := t.TempDir()
	if err := os.Symlink(shPath, filepath.Join(dir, "sh")); err != nil {
		t.Fatalf("Symlink(sh): %v", err)
	}

	if includeDD {
		ddPath, err := exec.LookPath("dd")
		if err != nil {
			t.Skipf("dd not found on PATH: %v", err)
		}
		if err := os.Symlink(ddPath, filepath.Join(dir, "dd")); err != nil {
			t.Fatalf("Symlink(dd): %v", err)
		}
	}
	return dir
}

func writeSSHCaptureScript(t *testing.T, dir, carriedName, envCapturePath, settingCapturePath string) string {
	t.Helper()
	content := "case \"$1\" in\n  --version) echo 'opencode v2.0.18'; exit 0;;\nesac\n" +
		"printf '%s' \"$" + carriedName + "\" > '" + envCapturePath + "'\n" +
		"printf '%s' \"$OPENCODE_DISABLE_AUTOUPDATE\" > '" + settingCapturePath + "'\n"
	return agenttest.WriteScript(t, dir, "fake-opencode-ssh", content)
}
func TestRunTurn_SSH_CarriesEnvironmentVariableAndSetting(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	tmpDir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioOpencodeSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "OPENCODE_TEST_CARRY_RUNTURN"
	const carriedValue = "carried-value-runturn"
	t.Setenv(carriedName, carriedValue)

	envCapturePath := filepath.Join(tmpDir, "env.out")
	settingCapturePath := filepath.Join(tmpDir, "setting.out")
	agentScript := writeSSHCaptureScript(t, tmpDir, carriedName, envCapturePath, settingCapturePath)

	a := &OpenCodeAdapter{}
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: agentScript},
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	a.RunTurn(context.Background(), session, domain.RunTurnParams{ //nolint:errcheck // disposition not under test here
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})

	gotEnv, err := os.ReadFile(envCapturePath)
	if err != nil {
		t.Fatalf("ReadFile(env.out): %v", err)
	}
	if string(gotEnv) != carriedValue {
		t.Errorf("carried variable = %q, want %q", string(gotEnv), carriedValue)
	}

	gotSetting, err := os.ReadFile(settingCapturePath)
	if err != nil {
		t.Fatalf("ReadFile(setting.out): %v", err)
	}
	if string(gotSetting) != "true" {
		t.Errorf("OPENCODE_DISABLE_AUTOUPDATE = %q, want %q", string(gotSetting), "true")
	}
}
