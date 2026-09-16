//go:build unix

package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// scenarioOpencodeSSHStandIn names the Go fake runtime registered
// below into fakeScenarios.
const scenarioOpencodeSSHStandIn = "opencode.ssh-stand-in"

func init() {
	fakeScenarios[scenarioOpencodeSSHStandIn] = agenttest.Typed(runOpencodeSSHStandIn)
}

// sshStandInParams configures [runOpencodeSSHStandIn]: PATH is the
// PATH value its dropped-environment child receives.
type sshStandInParams struct {
	PATH string
}

// runOpencodeSSHStandIn is a stand-in "ssh" runtime: it ignores every
// argument ahead of the last one, the remote command
// sshutil.BuildSSHLaunch produced, and runs that command through sh -c
// with its own environment dropped and replaced by params.PATH alone,
// and its standard input, output, and error inherited. Dropping the
// environment is what proves a carried variable or setting reaches the
// remote command only through the SSH session's standard input, never
// through this process's own inherited environment.
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

// sshStandInEnvPath returns the PATH value the ssh stand-in's dropped-
// environment child should receive: a directory holding only a
// symlink to sh, and, when includeDD is true, a symlink to dd as well.
// Building an isolated directory rather than reusing sh's own
// directory matters because a real sh and a real dd usually share one
// directory (/usr/bin, /bin), so reusing it for the no-dd case would
// resolve dd anyway.
func sshStandInEnvPath(t *testing.T, includeDD bool) string {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH: %v", err)
	}

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

// writeSSHCaptureScript writes a script that captures carriedName's
// value and the OPENCODE_AUTO_SHARE managed setting's value, each to
// its own file, then exits 0.
func writeSSHCaptureScript(t *testing.T, dir, carriedName, envCapturePath, settingCapturePath string) string {
	t.Helper()
	content := "printf '%s' \"$" + carriedName + "\" > '" + envCapturePath + "'\n" +
		"printf '%s' \"$OPENCODE_AUTO_SHARE\" > '" + settingCapturePath + "'\n"
	return agenttest.WriteScript(t, dir, "fake-opencode-ssh", content)
}

// pollOpencodePIDAndAssertGone polls path for a positive PID, then
// polls until kill(pid, 0) reports an error (process gone), failing t
// if either bound is exceeded.
func pollOpencodePIDAndAssertGone(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && v > 0 {
				pid = v
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("pid file %q never populated", path)
	}

	goneDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(goneDeadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant %d still answers signal 0, want it gone", pid)
}

// TestQueryExportUsage_HeldDescendantHoldingOutput asserts that with
// a held descendant holding the export query's output, queryExportUsage
// returns within its timer with the export's usage, and the descendant
// is gone.
func TestQueryExportUsage_HeldDescendantHoldingOutput(t *testing.T) {
	tmpDir := t.TempDir()
	fixturePath := filepath.Join(tmpDir, "export_usage.json")
	if err := os.WriteFile(fixturePath, loadFixture(t, "export_usage.json"), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
		t.Fatalf("WriteFile(export_usage.json): %v", err)
	}
	pidPath := filepath.Join(tmpDir, "descendant.pid")

	body := "cat '" + fixturePath + "'\n" +
		"sleep 30 & echo $! > '" + pidPath + "'\n"
	script := agenttest.WriteScript(t, tmpDir, "fake-export", body)
	state := testExportState(script, tmpDir)

	start := time.Now()
	usage := queryExportUsage(context.Background(), state, 0)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryExportUsage() took %v, want within 3s", elapsed)
	}
	if usage.InputTokens != 1750 {
		t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
	}
	if usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
	}

	pollOpencodePIDAndAssertGone(t, pidPath)
}

// TestQueryModelNotFound_HeldDescendantHoldingOutput asserts that with
// a held descendant holding the models query's output,
// queryModelNotFound returns within its timer reporting the configured
// model present in the catalog (ok false), and the descendant is gone.
func TestQueryModelNotFound_HeldDescendantHoldingOutput(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "descendant.pid")

	body := "printf 'anthropic/claude-sonnet-4-5\\nopenai/gpt-5\\n'\n" +
		"sleep 30 & echo $! > '" + pidPath + "'\n"
	script := agenttest.WriteScript(t, tmpDir, "fake-models", body)

	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       script,
			WorkspacePath: tmpDir,
		},
		baseLogger: slog.Default(),
	}
	state.passthrough.Model = "anthropic/claude-sonnet-4-5"

	start := time.Now()
	message, ok := queryModelNotFound(context.Background(), state)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryModelNotFound() took %v, want within 3s", elapsed)
	}
	if ok {
		t.Errorf("ok = true (message %q), want false: the configured model is present in the catalog", message)
	}

	pollOpencodePIDAndAssertGone(t, pidPath)
}

func writeExportScript(t *testing.T, dir, fixtureName string, exitCode int) (string, string) {
	t.Helper()

	argsPath := filepath.Join(dir, "args.log")
	body := `printf '%s\n' "$@" > '` + argsPath + `'
exit ` + strconv.Itoa(exitCode)
	if fixtureName != "" && exitCode == 0 {
		fixturePath := filepath.Join(dir, fixtureName)
		if err := os.WriteFile(fixturePath, loadFixture(t, fixtureName), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", fixtureName, err)
		}
		body = `printf '%s\n' "$@" > '` + argsPath + `'
cat '` + fixturePath + `'`
	}

	return agenttest.WriteScript(t, dir, "fake-export", body), argsPath
}

func testExportState(command, workspace string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			Command:       command,
			WorkspacePath: workspace,
		},
		sessionID:  "ses_abc123",
		baseLogger: slog.Default(),
	}
}

func TestQueryExportSubprocess(t *testing.T) {
	t.Parallel()

	t.Run("a_cancelled_turn_context_does_not_cost_the_recovery", func(t *testing.T) {
		t.Parallel()

		// Every terminal path hands `recoverUsage` the TURN's context, and on
		// the cancel path that context is the thing that just fired. The read
		// timeout and the process-exit path can reach it after a cancellation
		// too. The export is terminal work about a turn that already ran, so
		// it must not inherit the caller's cancellation.
		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// The hazard, measured rather than assumed: an attached query on this
		// context recovers nothing at all.
		if usage := queryExportUsage(ctx, state, 0); hasUsage(usage) {
			t.Fatal("queryExportUsage recovered on a cancelled context; this test's premise is gone")
		}

		recovered := recoverUsage(ctx, state, 0)
		if recovered == nil {
			t.Fatal("recoverUsage = nil on a cancelled turn context, want the export's figures")
		}
		if recovered.Run.InputTokens != 1750 || recovered.Run.OutputTokens != 300 {
			t.Errorf("Run = in:%d out:%d, want in:1750 out:300",
				recovered.Run.InputTokens, recovered.Run.OutputTokens)
		}
	})

	t.Run("the_warning_reports_an_export_that_recovered_nothing", func(t *testing.T) {
		t.Parallel()

		// A finished step whose provider reported zero is a measurement, so
		// saying no usage was found contradicts the verdict it produces. The
		// unfinished placeholder recovers nothing and must still say so.
		for name, tc := range map[string]struct {
			finish   string
			wantWarn bool
		}{
			"finished_zero_step": {finish: `"finish":"stop",`, wantWarn: false},
			"unfinished_step":    {finish: ``, wantWarn: true},
		} {
			tmpDir := t.TempDir()
			exportPath := filepath.Join(tmpDir, "export.json")
			export := `{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123",` + tc.finish +
				`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}]}`
			if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
				t.Fatal(err)
			}
			script := agenttest.WriteScript(t, tmpDir, "fake-export", "cat '"+exportPath+"'")

			var logs bytes.Buffer
			state := testExportState(script, tmpDir)
			state.baseLogger = slog.New(slog.NewTextHandler(&logs, nil))

			queryExportUsage(context.Background(), state, 0)

			if got := strings.Contains(logs.String(), "no assistant token usage found"); got != tc.wantWarn {
				t.Errorf("%s: warned = %v, want %v", name, got, tc.wantWarn)
			}
		}
	})

	t.Run("local_subprocess_usage_extracted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, argsPath := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
		}
		if usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.CacheReadTokens)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("ReadFile(args.log): %v", err)
		}
		if string(args) != "export\n--sanitize\nses_abc123\n" {
			t.Errorf("export args = %q, want %q", string(args), "export\n--sanitize\nses_abc123\n")
		}
	})

	t.Run("local_subprocess_missing_tokens_returns_zero", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "export_usage_missing_tokens.json", 0)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value", usage)
		}
	})

	t.Run("local_subprocess_nonzero_exit_returns_zero", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "", 1)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value", usage)
		}
	})

	t.Run("ssh_subprocess_usage_extracted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, argsPath := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)
		state.target.RemoteCommand = "opencode"
		state.target.SSHHost = "example.test"

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
		}
		if usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.CacheReadTokens)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("ReadFile(args.log): %v", err)
		}
		logged := string(args)
		if !strings.Contains(logged, "example.test") {
			t.Errorf("ssh args = %q, want host %q", logged, "example.test")
		}
		if !strings.Contains(logged, "export") || !strings.Contains(logged, "--sanitize") || !strings.Contains(logged, "ses_abc123") {
			t.Errorf("ssh args = %q, want export invocation details", logged)
		}
	})
}

// TestQueryExportUsage_SSH_CarriesEnvironmentVariableAndSetting drives
// queryExportUsage's remote branch through a stand-in ssh runtime and
// asserts that the fake remote command observes both a carried
// variable's value and the OPENCODE_AUTO_SHARE managed setting,
// delivered only through the SSH session's standard input rather than
// through the stand-in's own inherited environment.
//
// Not run with t.Parallel(): sets PATH and the carried variable via
// t.Setenv.
func TestQueryExportUsage_SSH_CarriesEnvironmentVariableAndSetting(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioOpencodeSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "OPENCODE_TEST_CARRY_EXPORT"
	const carriedValue = "carried-value-export"
	t.Setenv(carriedName, carriedValue)

	envCapturePath := filepath.Join(tmpDir, "env.out")
	settingCapturePath := filepath.Join(tmpDir, "setting.out")
	agentScript := writeSSHCaptureScript(t, tmpDir, carriedName, envCapturePath, settingCapturePath)

	state := testExportState(agentScript, tmpDir)
	state.target.RemoteCommand = agentScript
	state.target.SSHHost = "user@stand-in-host"
	state.target.SSHEnvNames = []string{carriedName}

	queryExportUsage(context.Background(), state, 0)

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
	if string(gotSetting) != "false" {
		t.Errorf("OPENCODE_AUTO_SHARE = %q, want %q", string(gotSetting), "false")
	}
}

// TestQueryModelNotFound_SSH_CarriesEnvironmentVariableAndSetting
// mirrors TestQueryExportUsage_SSH_CarriesEnvironmentVariableAndSetting
// for queryModelNotFound's remote branch.
//
// Not run with t.Parallel(): sets PATH and the carried variable via
// t.Setenv.
func TestQueryModelNotFound_SSH_CarriesEnvironmentVariableAndSetting(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioOpencodeSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "OPENCODE_TEST_CARRY_MODELS"
	const carriedValue = "carried-value-models"
	t.Setenv(carriedName, carriedValue)

	envCapturePath := filepath.Join(tmpDir, "env.out")
	settingCapturePath := filepath.Join(tmpDir, "setting.out")
	agentScript := writeSSHCaptureScript(t, tmpDir, carriedName, envCapturePath, settingCapturePath)

	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       agentScript,
			WorkspacePath: tmpDir,
			RemoteCommand: agentScript,
			SSHHost:       "user@stand-in-host",
			SSHEnvNames:   []string{carriedName},
		},
		baseLogger: slog.Default(),
	}
	state.passthrough.Model = "anthropic/claude-sonnet-4-5"

	queryModelNotFound(context.Background(), state)

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
	if string(gotSetting) != "false" {
		t.Errorf("OPENCODE_AUTO_SHARE = %q, want %q", string(gotSetting), "false")
	}
}

// TestRunTurn_SSH_CarriesEnvironmentVariableAndSetting drives
// OpenCodeAdapter.RunTurn's remote branch through a stand-in ssh
// runtime and asserts that the fake remote command observes both a
// carried variable's value and the OPENCODE_AUTO_SHARE managed
// setting.
//
// Not run with t.Parallel(): sets PATH and the carried variable via
// t.Setenv.
func TestRunTurn_SSH_CarriesEnvironmentVariableAndSetting(t *testing.T) {
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

	// The turn's own disposition is not this test's subject: the fake
	// remote command emits no opencode JSON at all, so whatever
	// RunTurn reports for that is exercised elsewhere. Only the
	// carried variable and setting matter here.
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
	if string(gotSetting) != "false" {
		t.Errorf("OPENCODE_AUTO_SHARE = %q, want %q", string(gotSetting), "false")
	}
}

// writeExportScriptWithStdinCapture is [writeExportScript] extended to
// record the whole of standard input the subprocess received, ahead
// of recording its argument vector.
func writeExportScriptWithStdinCapture(t *testing.T, dir, fixtureName string, exitCode int, stdinPath string) (string, string) {
	t.Helper()

	argsPath := filepath.Join(dir, "args.log")
	body := `cat > '` + stdinPath + `'
printf '%s\n' "$@" > '` + argsPath + `'
exit ` + strconv.Itoa(exitCode)
	if fixtureName != "" && exitCode == 0 {
		fixturePath := filepath.Join(dir, fixtureName)
		if err := os.WriteFile(fixturePath, loadFixture(t, fixtureName), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", fixtureName, err)
		}
		body = `cat > '` + stdinPath + `'
printf '%s\n' "$@" > '` + argsPath + `'
cat '` + fixturePath + `'`
	}

	return agenttest.WriteScript(t, dir, "fake-export", body), argsPath
}

// TestQueryExportUsage_LocalLaunchIgnoresSSHEnvNames asserts that a
// local export query (RemoteCommand empty) sends the same argument
// vector and empty standard input whether or not
// LaunchTarget.SSHEnvNames names a set variable: queryExportUsage's
// local branch never consults it. This reddens if that branch starts
// treating a non-empty SSHEnvNames as a signal to take the remote
// path, which would replace the local argument vector with an SSH
// option vector and attach a non-empty preamble to standard input.
func TestQueryExportUsage_LocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	// Not parallel: sets the carried variable via t.Setenv.
	const varName = "SORTIE_OPENCODE_EXPORT_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	for _, tc := range []struct {
		name        string
		sshEnvNames []string
	}{
		{"SSHEnvNames absent", nil},
		{"SSHEnvNames naming a set variable", []string{varName}},
	} {
		tmpDir := t.TempDir()
		stdinPath := filepath.Join(tmpDir, "stdin.txt")
		script, argsPath := writeExportScriptWithStdinCapture(t, tmpDir, "export_usage.json", 0, stdinPath)
		state := testExportState(script, tmpDir)
		state.target.SSHEnvNames = tc.sshEnvNames

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.InputTokens != 1750 || usage.OutputTokens != 300 {
			t.Errorf("%s: usage = %+v, want InputTokens=1750 OutputTokens=300", tc.name, usage)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(args.log): %v", tc.name, err)
		}
		const wantArgs = "export\n--sanitize\nses_abc123\n"
		if string(args) != wantArgs {
			t.Errorf("%s: export args = %q, want %q", tc.name, string(args), wantArgs)
		}

		stdin, err := os.ReadFile(stdinPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(stdin.txt): %v", tc.name, err)
		}
		if len(stdin) != 0 {
			t.Errorf("%s: subprocess standard input = %q, want empty on a local launch", tc.name, stdin)
		}
	}
}

// TestQueryModelNotFound_LocalLaunchIgnoresSSHEnvNames mirrors
// [TestQueryExportUsage_LocalLaunchIgnoresSSHEnvNames] for
// queryModelNotFound's local branch.
func TestQueryModelNotFound_LocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	// Not parallel: sets the carried variable via t.Setenv.
	const varName = "SORTIE_OPENCODE_MODELS_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	for _, tc := range []struct {
		name        string
		sshEnvNames []string
	}{
		{"SSHEnvNames absent", nil},
		{"SSHEnvNames naming a set variable", []string{varName}},
	} {
		tmpDir := t.TempDir()
		argvPath := filepath.Join(tmpDir, "argv.txt")
		stdinPath := filepath.Join(tmpDir, "stdin.txt")
		body := `cat > '` + stdinPath + `'
printf '%s\n' "$@" > '` + argvPath + `'
printf 'anthropic/claude-sonnet-4-5\nopenai/gpt-5\n'
`
		script := agenttest.WriteScript(t, tmpDir, "fake-models", body)

		state := &sessionState{
			target: agentcore.LaunchTarget{
				Command:       script,
				WorkspacePath: tmpDir,
				SSHEnvNames:   tc.sshEnvNames,
			},
			baseLogger: slog.Default(),
		}
		state.passthrough.Model = "anthropic/claude-sonnet-4-5"

		_, ok := queryModelNotFound(context.Background(), state)
		if ok {
			t.Errorf("%s: ok = true, want false: the configured model is present in the catalog", tc.name)
		}

		argv, err := os.ReadFile(argvPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(argv.txt): %v", tc.name, err)
		}
		const wantArgv = "models\n"
		if string(argv) != wantArgv {
			t.Errorf("%s: models argv = %q, want %q", tc.name, string(argv), wantArgv)
		}

		stdin, err := os.ReadFile(stdinPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(stdin.txt): %v", tc.name, err)
		}
		if len(stdin) != 0 {
			t.Errorf("%s: subprocess standard input = %q, want empty on a local launch", tc.name, stdin)
		}
	}
}
