//go:build unix

package clientprotocol

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// geminiProbeMarkerTimeout bounds each deterministic wait for a probe's
// public started marker.
const geminiProbeMarkerTimeout = 10 * time.Second

// geminiQualificationWorkspace is the controlled fixture tree one live
// run owns: a git-initialized controlled checkout the runtime's working
// directory points at, and run-scoped HOME and GEMINI_CLI_HOME classes
// under the same temporary root. Absolute paths never reach evidence;
// only their classes do.
type geminiQualificationWorkspace struct {
	Checkout string
	Home     string
	CLIHome  string
}

// geminiNewQualificationWorkspace builds the controlled workspace: a
// git checkout and two run-scoped configuration homes under t.TempDir().
// The checkout starts with no project-scoped Gemini configuration, and
// the workspace tests keep that a fixture fact rather than an
// assumption.
func geminiNewQualificationWorkspace(t *testing.T) geminiQualificationWorkspace {
	t.Helper()

	root := t.TempDir()
	workspace := geminiQualificationWorkspace{
		Checkout: gitInitWorkspace(t),
		Home:     filepath.Join(root, "run-home"),
		CLIHome:  filepath.Join(root, "run-cli-home"),
	}
	for _, dir := range []string{workspace.Home, workspace.CLIHome} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create run-scoped home %s: %v", dir, err)
		}
	}
	return workspace
}

// geminiProjectGeminiConfigNames are the project-scoped Gemini
// configuration paths whose presence the workspace fixture forbids,
// except for the explicit test MCP input delivered through Sortie.
var geminiProjectGeminiConfigNames = []string{
	"settings.json", "settings.yaml", "hooks", "extensions", "commands",
	"mcp.json", ".mcp.json",
}

// geminiPresentProjectGeminiConfig reports every project-scoped Gemini
// configuration path that exists under the checkout, so a fixture that
// accidentally carries one fails rather than silently loading
// operator-controlled configuration.
func geminiPresentProjectGeminiConfig(checkout string) []string {
	present := []string{}
	entries, err := os.ReadDir(filepath.Join(checkout, ".gemini"))
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if slices.Contains(geminiProjectGeminiConfigNames, entry.Name()) {
			present = append(present, filepath.Join(".gemini", entry.Name()))
		}
	}
	if _, err := os.Stat(filepath.Join(checkout, ".mcp.json")); err == nil {
		present = append(present, ".mcp.json")
	}
	return present
}

// geminiAssertNoProjectGeminiConfig fails t when the controlled
// checkout carries any project-scoped Gemini configuration the fixture
// did not declare.
func geminiAssertNoProjectGeminiConfig(t *testing.T, checkout string) {
	t.Helper()
	if present := geminiPresentProjectGeminiConfig(checkout); len(present) > 0 {
		t.Errorf("controlled checkout carries undeclared project Gemini configuration: %v", present)
	}
}

// geminiProbeSpec describes one local probe executable's whole bounded
// behavior: its basename, whether it writes a public started marker,
// its fixed exit code, and whether it waits after the marker.
type geminiProbeSpec struct {
	name     string
	marker   bool
	exitCode int
	wait     bool
}

// geminiLocalProbeSpecs returns the five local probe descriptors in
// fixed order. Every behavior comes from the probe input contracts:
// the policy-load and permission probes perform no side effect, the
// failing probe writes its marker and exits 23, and the cancellation
// and transport probes write their markers and wait.
func geminiLocalProbeSpecs() []geminiProbeSpec {
	return []geminiProbeSpec{
		{name: "policy-load-probe", marker: false, exitCode: 0, wait: false},
		{name: "permission-probe", marker: false, exitCode: 0, wait: false},
		{name: "failing-probe", marker: true, exitCode: 23, wait: false},
		{name: "cancellation-probe", marker: true, exitCode: 0, wait: true},
		{name: "transport-probe", marker: true, exitCode: 0, wait: true},
	}
}

// geminiQualificationProbes carries the absolute paths of the five
// local probe executables under the controlled workspace.
type geminiQualificationProbes struct {
	PolicyLoad   string
	Permission   string
	Failing      string
	Cancellation string
	Transport    string
}

// paths returns the five probe absolute paths in fixed order.
func (p geminiQualificationProbes) paths() []string {
	return []string{p.PolicyLoad, p.Permission, p.Failing, p.Cancellation, p.Transport}
}

// names returns the five probe basenames, the only helper process
// basenames the workspace-security allowlist admits.
func (p geminiQualificationProbes) names() []string {
	names := make([]string, 0, len(p.paths()))
	for _, path := range p.paths() {
		names = append(names, filepath.Base(path))
	}
	return names
}

// geminiWriteProbeExecutables writes the five local probe executables
// under the controlled workspace and returns their absolute paths.
// Every probe performs no network access and no mutation beyond its
// own started marker.
func geminiWriteProbeExecutables(t *testing.T, workspace string) geminiQualificationProbes {
	t.Helper()

	write := func(spec geminiProbeSpec) string {
		t.Helper()
		var body strings.Builder
		if spec.marker {
			fmt.Fprintf(&body, "echo started > '%s'\n", geminiProbeMarkerPath(filepath.Join(workspace, spec.name)))
		}
		if spec.wait {
			body.WriteString("sleep 30\n")
		} else {
			fmt.Fprintf(&body, "exit %d\n", spec.exitCode)
		}
		return agenttest.WriteScript(t, workspace, spec.name, body.String())
	}

	written := make([]string, 0, 5)
	for _, spec := range geminiLocalProbeSpecs() {
		written = append(written, write(spec))
	}
	return geminiQualificationProbes{
		PolicyLoad:   written[0],
		Permission:   written[1],
		Failing:      written[2],
		Cancellation: written[3],
		Transport:    written[4],
	}
}

// geminiProbeMarkerPath returns a probe's public started-marker path.
func geminiProbeMarkerPath(probePath string) string {
	return probePath + ".started"
}

// geminiUniqueMarker generates one unique, non-secret deny marker for
// the policy precondition. It is a random public token: nothing about
// a credential, session, or host value leaks through it.
func geminiUniqueMarker() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "sortie-qualification-deny-marker-unavailable"
	}
	return "sortie-qualification-deny-" + hex.EncodeToString(raw[:])
}

// geminiWriteQualificationPolicy writes the run-scoped policy file with
// exactly the five high-priority rules the qualification contract
// defines: deny with a unique marker on the policy-load probe,
// ask_user on the permission probe, and allow on the failing,
// cancellation, and transport probes. No other command is allowed by
// this policy. It returns the policy path and the deny marker.
func geminiWriteQualificationPolicy(t *testing.T, dir string, probes geminiQualificationProbes) (path string, denyMarker string) {
	t.Helper()

	denyMarker = geminiUniqueMarker()
	rules := []struct {
		prefix   string
		decision string
	}{
		{probes.PolicyLoad, "deny"},
		{probes.Permission, "ask_user"},
		{probes.Failing, "allow"},
		{probes.Cancellation, "allow"},
		{probes.Transport, "allow"},
	}

	var body strings.Builder
	body.WriteString("# Run-scoped qualification policy: five high-priority rules and nothing else.\n")
	for _, rule := range rules {
		fmt.Fprintf(&body,
			"\n[[rule]]\ntoolName = \"run_shell_command\"\ncommandPrefix = %q\ndecision = %q\npriority = 100\n",
			rule.prefix, rule.decision)
		if rule.decision == "deny" {
			fmt.Fprintf(&body, "denyMessage = %q\n", denyMarker)
		}
	}

	path = filepath.Join(dir, "qualification-policy.toml")
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write qualification policy: %v", err)
	}
	return path, denyMarker
}

// geminiQualificationLaunchArgv builds one qualification launch's argv.
// Every qualification launch carries exactly the operator-selected
// model, the default approval mode, the run-scoped policy, and
// --skip-trust; the protocol surface differs by --acp and the native
// surfaces by --output-format plus the headless --prompt entry point.
// No other flag is appended to any qualification launch.
func geminiQualificationLaunchArgv(config geminiQualificationConfig, surface qualification.Surface, prompt string, policyPath string) []string {
	argv := []string{
		config.CommandPath,
		"--model", config.Model,
		"--approval-mode", "default",
		"--policy", policyPath,
		"--skip-trust",
	}
	switch surface {
	case qualification.SurfaceProtocol:
		return append(argv, "--acp")
	case qualification.SurfaceNativeText:
		return append(argv, "--output-format", "text", "--prompt", prompt)
	case qualification.SurfaceNativeJSON:
		return append(argv, "--output-format", "json", "--prompt", prompt)
	case qualification.SurfaceNativeStreamJSON:
		return append(argv, "--output-format", "stream-json", "--prompt", prompt)
	}
	return argv
}

// geminiQualificationVersionArgv builds the one-shot version capture's
// argv. It is a coordinate-resolution command, not a qualification
// launch, so it carries none of the qualification flags.
func geminiQualificationVersionArgv(config geminiQualificationConfig) []string {
	return []string{config.CommandPath, "--version"}
}

// TestGeminiQualificationPolicyFixture confirms the run-scoped policy
// file carries exactly the five high-priority rules, with the unique
// deny marker on the policy-load probe, ask_user on the permission
// probe, allow on the three behavioral probes, and no rule for any
// other command. It also pins the qualification launch argv shape.
func TestGeminiQualificationPolicyFixture(t *testing.T) {
	t.Parallel()

	workspace := geminiNewQualificationWorkspace(t)
	probes := geminiWriteProbeExecutables(t, workspace.Checkout)
	policyPath, denyMarker := geminiWriteQualificationPolicy(t, t.TempDir(), probes)

	t.Run("policy carries exactly the five rules", func(t *testing.T) {
		t.Parallel()

		raw, err := os.ReadFile(policyPath)
		if err != nil {
			t.Fatalf("read policy fixture: %v", err)
		}
		content := string(raw)
		if got := strings.Count(content, "[[rule]]"); got != 5 {
			t.Errorf("policy rule count = %d, want exactly 5", got)
		}
		for i, prefix := range []string{probes.PolicyLoad, probes.Permission, probes.Failing, probes.Cancellation, probes.Transport} {
			if !strings.Contains(content, fmt.Sprintf("commandPrefix = %q", prefix)) {
				t.Errorf("policy carries no rule for probe %d", i+1)
			}
		}
		if got := strings.Count(content, `decision = "deny"`); got != 1 {
			t.Errorf("deny decision count = %d, want exactly 1", got)
		}
		if got := strings.Count(content, `decision = "ask_user"`); got != 1 {
			t.Errorf("ask_user decision count = %d, want exactly 1", got)
		}
		if got := strings.Count(content, `decision = "allow"`); got != 3 {
			t.Errorf("allow decision count = %d, want exactly 3", got)
		}
		if got := strings.Count(content, "priority = 100"); got != 5 {
			t.Errorf("high-priority rule count = %d, want 5", got)
		}
		if !strings.Contains(content, fmt.Sprintf("denyMessage = %q", denyMarker)) {
			t.Errorf("policy carries no deny message equal to the generated marker")
		}
		if strings.Count(content, denyMarker) != 1 {
			t.Errorf("deny marker appears %d times, want one unique marker", strings.Count(content, denyMarker))
		}
	})

	t.Run("deny marker is unique per run and is a bare public token", func(t *testing.T) {
		t.Parallel()

		_, otherMarker := geminiWriteQualificationPolicy(t, t.TempDir(), probes)
		if otherMarker == denyMarker {
			t.Error("two policy fixtures generated the same deny marker, want a unique marker per run")
		}
		if strings.ContainsAny(denyMarker, " \t=/\\") {
			t.Errorf("deny marker %q is not a bare token", denyMarker)
		}
	})

	t.Run("protocol launch argv carries exactly the qualification flags", func(t *testing.T) {
		t.Parallel()

		config := geminiQualificationConfig{CommandPath: "/opt/gemini", Model: "gemini-fixture-model"}
		got := geminiQualificationLaunchArgv(config, qualification.SurfaceProtocol, "unused-prompt", policyPath)
		want := []string{
			config.CommandPath,
			"--model", config.Model,
			"--approval-mode", "default",
			"--policy", policyPath,
			"--skip-trust",
			"--acp",
		}
		if !slices.Equal(got, want) {
			t.Errorf("protocol argv = %v, want %v", got, want)
		}
	})

	t.Run("native launch argv differs only by output format and prompt", func(t *testing.T) {
		t.Parallel()

		config := geminiQualificationConfig{CommandPath: "/opt/gemini", Model: "gemini-fixture-model"}
		prompt := "Reply with exactly SORTIE_BASELINE_OK and do not call any tool."
		surfaces := []struct {
			surface qualification.Surface
			format  string
		}{
			{qualification.SurfaceNativeText, "text"},
			{qualification.SurfaceNativeJSON, "json"},
			{qualification.SurfaceNativeStreamJSON, "stream-json"},
		}
		for _, tt := range surfaces {
			argv := geminiQualificationLaunchArgv(config, tt.surface, prompt, policyPath)
			want := []string{
				config.CommandPath,
				"--model", config.Model,
				"--approval-mode", "default",
				"--policy", policyPath,
				"--skip-trust",
				"--output-format", tt.format,
				"--prompt", prompt,
			}
			if !slices.Equal(argv, want) {
				t.Errorf("%s argv = %v, want %v", tt.surface, argv, want)
			}
			if slices.Contains(argv, "--acp") {
				t.Errorf("%s argv carries the protocol-only --acp flag: %v", tt.surface, argv)
			}
		}
	})

	t.Run("version capture argv carries no qualification flag", func(t *testing.T) {
		t.Parallel()

		config := geminiQualificationConfig{CommandPath: "/opt/gemini"}
		argv := geminiQualificationVersionArgv(config)
		if !slices.Equal(argv, []string{"/opt/gemini", "--version"}) {
			t.Errorf("version argv = %v, want only the executable and --version", argv)
		}
	})
}

// TestGeminiQualificationWorkspaceFixture confirms the controlled
// workspace: a git checkout, run-scoped HOME and GEMINI_CLI_HOME under
// the same temporary root, and no project-scoped Gemini configuration
// before launch.
func TestGeminiQualificationWorkspaceFixture(t *testing.T) {
	t.Parallel()

	workspace := geminiNewQualificationWorkspace(t)

	t.Run("checkout is a git work tree", func(t *testing.T) {
		t.Parallel()

		if _, err := os.Stat(filepath.Join(workspace.Checkout, ".git")); err != nil {
			t.Errorf("stat .git: %v", err)
		}
	})

	t.Run("home coordinates are run-scoped temporary classes", func(t *testing.T) {
		t.Parallel()

		if filepath.Dir(workspace.Home) != filepath.Dir(workspace.CLIHome) {
			t.Errorf("run-scoped homes resolve under different roots: %q vs %q",
				filepath.Dir(workspace.Home), filepath.Dir(workspace.CLIHome))
		}
		for _, dir := range []string{workspace.Home, workspace.CLIHome} {
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("run-scoped home class does not exist: %v", err)
			}
		}
	})

	t.Run("checkout carries no project-scoped Gemini configuration", func(t *testing.T) {
		t.Parallel()

		geminiAssertNoProjectGeminiConfig(t, workspace.Checkout)
	})

	t.Run("a planted configuration is detected", func(t *testing.T) {
		t.Parallel()

		planted := geminiNewQualificationWorkspace(t)
		if err := os.MkdirAll(filepath.Join(planted.Checkout, ".gemini"), 0o750); err != nil {
			t.Fatalf("plant .gemini: %v", err)
		}
		for _, name := range []string{"settings.json", "settings.yaml", "mcp.json"} {
			path := filepath.Join(planted.Checkout, ".gemini", name)
			if err := os.WriteFile(path, []byte("# planted fixture configuration\n"), 0o600); err != nil {
				t.Fatalf("plant %s: %v", name, err)
			}
		}
		present := geminiPresentProjectGeminiConfig(planted.Checkout)
		want := []string{".gemini/mcp.json", ".gemini/settings.json", ".gemini/settings.yaml"}
		if !slices.Equal(present, want) {
			t.Errorf("geminiPresentProjectGeminiConfig() = %v, want %v", present, want)
		}
	})
}

// TestGeminiQualificationLocalProbeFixtures confirms the five local
// probe executables exist under the controlled workspace and perform
// exactly their assigned bounded behavior: no side effect for the
// policy-load and permission probes, marker plus fixed exit 23 for the
// failing probe, and marker plus a bounded wait for the cancellation
// and transport probes. The wait probes stay sequential so each owns
// its process group alone.
func TestGeminiQualificationLocalProbeFixtures(t *testing.T) {
	workspace := geminiNewQualificationWorkspace(t)
	probes := geminiWriteProbeExecutables(t, workspace.Checkout)
	specs := geminiLocalProbeSpecs()

	t.Run("exactly five probe descriptors", func(t *testing.T) {
		t.Parallel()

		if len(specs) != 5 {
			t.Fatalf("probe descriptor count = %d, want 5", len(specs))
		}
		wantNames := []string{"policy-load-probe", "permission-probe", "failing-probe", "cancellation-probe", "transport-probe"}
		got := make([]string, 0, len(specs))
		for _, spec := range specs {
			got = append(got, spec.name)
		}
		if !slices.Equal(got, wantNames) {
			t.Errorf("probe names = %v, want %v", got, wantNames)
		}
	})

	t.Run("probe executables live under the controlled workspace", func(t *testing.T) {
		t.Parallel()

		for _, path := range probes.paths() {
			if filepath.Dir(path) != workspace.Checkout {
				t.Errorf("probe %s lives outside the controlled checkout", filepath.Base(path))
			}
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				t.Errorf("probe %s is not an executable file: %v", filepath.Base(path), err)
			}
		}
		if got := probes.names(); len(got) != 5 || slices.Contains(got, "") {
			t.Errorf("probe allowlist basenames = %v, want five basenames", got)
		}
	})

	for _, spec := range specs {
		t.Run("behavior of "+spec.name, func(t *testing.T) {
			// Sequential: this subtest launches and reaps its own probe
			// process group.
			path := filepath.Join(workspace.Checkout, spec.name)
			markerPath := geminiProbeMarkerPath(path)

			cmd := execGeminiFixtureProbe(t, path, workspace.Checkout)
			groupPID := cmd.Process.Pid

			if spec.wait {
				waitForFile(t, markerPath, geminiProbeMarkerTimeout)
				if err := procutil.SignalProcessGroup(groupPID, 0); err != nil {
					t.Fatalf("wait probe group died before cancellation: %v", err)
				}
				_ = procutil.SignalProcessGroup(groupPID, syscall.SIGKILL)
				_, _ = cmd.Process.Wait()
				qualification.AwaitProcessGroupAbsence(t, groupPID)
				return
			}

			waitErr := cmd.Wait()
			exitCode := geminiProbeExitCode(waitErr)
			if exitCode < 0 {
				t.Fatalf("probe %s did not terminate cleanly: %v", spec.name, waitErr)
			}
			_, markerErr := os.Stat(markerPath)
			switch {
			case spec.marker && markerErr != nil:
				t.Errorf("probe %s wrote no started marker: %v", spec.name, markerErr)
			case !spec.marker && markerErr == nil:
				t.Errorf("probe %s wrote a marker, want none", spec.name)
			}
			if exitCode != spec.exitCode {
				t.Errorf("probe %s exit code = %d, want %d", spec.name, exitCode, spec.exitCode)
			}
		})
	}
}

// execGeminiFixtureProbe starts one probe executable inside the
// checkout with its own process group, so its whole group is
// attributable and cleanable. The registered cleanup kills the group
// and reaps whatever the test itself did not wait for.
func execGeminiFixtureProbe(t *testing.T, path, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(path) //nolint:gosec // the probe path is written by this test under its own temp workspace
	cmd.Dir = dir
	procutil.SetProcessGroup(cmd)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start probe %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() {
		_ = procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// geminiProbeExitCode extracts the exit code from a Wait error, or -1
// when the process is still running or the error is not an exit.
func geminiProbeExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}
