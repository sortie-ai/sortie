//go:build unix

package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

const (
	// ambientEnvName is exported by an operator's shell but named by no
	// coordinate. It must reach neither compared surface, so a difference
	// between the two readings is a difference between the surfaces.
	ambientEnvName = "QUALIFICATION_AMBIENT_LEAK_PROBE"

	// configRootEnvName stands in for the profile's config-root coordinate, the
	// isolated root a launch must actually read.
	configRootEnvName = "QUALIFICATION_TEST_CONFIG_ROOT"

	// authEnvName stands in for an operator-supplied credential name; its value
	// is a credential, so no artifact may carry it.
	authEnvName = "QUALIFICATION_TEST_CREDENTIAL"
)

func envSurfaceCoordinates(runtimePath string) Coordinates {
	return Coordinates{
		CommandPath:  runtimePath,
		Model:        "fixture-model",
		AuthEnvNames: []string{authEnvName},
		Profile: profile.RuntimeProfile{
			RuntimeID:          "env-surface",
			ConfigRootEnvNames: []string{configRootEnvName},
			EntryPoints: map[evidence.Surface]profile.EntryPoint{
				evidence.SurfaceProtocol: {Args: []string{"--acp"}},
			},
		},
	}
}

func plantEnvironment(t *testing.T) (configRoot, credential string) {
	t.Helper()
	configRoot = t.TempDir()
	credential = "credential-" + filepath.Base(configRoot)
	t.Setenv(ambientEnvName, "1")
	t.Setenv(configRootEnvName, configRoot)
	t.Setenv(authEnvName, credential)
	return configRoot, credential
}

// envReportingACPScenario answers the handshake and one prompt, writing its own
// environment to ReportPath as its first action.
const envReportingACPScenario = "env-reporting-acp-agent"

type envReportingACPParams struct {
	ReportPath string
}

func runEnvReportingACPAgent(_ []string, params envReportingACPParams) int {
	if err := os.WriteFile(params.ReportPath, []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "report environment: %v\n", err)
		return 2
	}

	out := bufio.NewWriter(os.Stdout)
	respond := func(id json.RawMessage, payload string) int {
		if _, err := fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,%s}`+"\n", id, payload); err != nil {
			return 2
		}
		if err := out.Flush(); err != nil {
			return 2
		}
		return 0
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var header struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &header); err != nil || header.Method == "" || len(header.ID) == 0 {
			continue
		}
		var code int
		switch header.Method {
		case "initialize":
			code = respond(header.ID, `"result":{"protocolVersion":1,"agentCapabilities":{}}`)
		case "session/new":
			code = respond(header.ID, `"result":{"sessionId":"env-surface-session"}`)
		case "session/prompt":
			code = respond(header.ID, `"result":{"stopReason":"end_turn"}`)
		default:
			code = respond(header.ID, `"error":{"code":-32601,"message":"method not found"}`)
		}
		if code != 0 {
			return code
		}
	}
	return 0
}

func init() {
	probeScenarios[envReportingACPScenario] = agenttest.Typed(runEnvReportingACPAgent)
}

func environmentReport(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // path is this test's own temporary directory
	if err != nil {
		t.Fatalf("read the reported environment %q: %v", path, err)
	}
	reported := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			reported[name] = value
		}
	}
	return reported
}

func TestComparedSurfacesLaunchUnderOneEnvironment(t *testing.T) {
	configRoot, credential := plantEnvironment(t)
	coords := envSurfaceCoordinates("")
	env := launchEnvironment(coords)

	t.Run("the native allowlist carries the coordinates and nothing ambient", func(t *testing.T) {
		if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, ambientEnvName+"=") }) {
			t.Errorf("launchEnvironment(...) carries %s, want it absent: no coordinate names it", ambientEnvName)
		}
		if !slices.Contains(env, configRootEnvName+"="+configRoot) {
			t.Errorf("launchEnvironment(...) does not carry %s=%s, want the isolated root the profile names", configRootEnvName, configRoot)
		}
		if !slices.Contains(env, authEnvName+"="+credential) {
			t.Errorf("launchEnvironment(...) does not carry %s, want the credential the coordinates name", authEnvName)
		}
	})

	t.Run("the protocol launch carries the same names", func(t *testing.T) {
		dir := t.TempDir()
		wrapper := writeLaunchEnvWrapper(t, dir, launchEnvNames(coords), "")
		reportPath := filepath.Join(dir, "wrapper-environment.txt")

		output, err := launchNativeProbe(t, wrapper,
			[]string{"/bin/sh", "-c", "env > " + reportPath}, dir, nil, newOwnedDescendants(&groupTracker{}))
		if err != nil {
			t.Fatalf("run the launch wrapper: %v; output: %q", err, output)
		}

		reported := environmentReport(t, reportPath)
		if _, carried := reported[ambientEnvName]; carried {
			t.Errorf("the wrapped launch carries %s, want it absent: the native surface never sees it", ambientEnvName)
		}
		if got := reported[configRootEnvName]; got != configRoot {
			t.Errorf("the wrapped launch carries %s=%q, want %q: the isolated root must reach the launch", configRootEnvName, got, configRoot)
		}
		if got := reported[authEnvName]; got != credential {
			t.Errorf("the wrapped launch carries %s=%q, want the credential the coordinates name", authEnvName, got)
		}
	})
}

func TestInductionSessionLaunchesUnderTheAllowlist(t *testing.T) {
	configRoot, credential := plantEnvironment(t)

	dir := t.TempDir()
	reportPath := filepath.Join(dir, "session-environment.txt")
	script := agenttest.FakeRuntime(t, dir, "acp-agent", envReportingACPScenario, envReportingACPParams{ReportPath: reportPath})
	coords := envSurfaceCoordinates(script)

	fixture := &sharedFixture{
		workspaceRoot: t.TempDir(),
		tracker:       &groupTracker{},
		usage:         &usageTracker{},
		env:           launchEnvironment(coords),
		envWrapper:    writeLaunchEnvWrapper(t, dir, launchEnvNames(coords), ""),
	}
	adapter, session, err := startInductionSession(t, coords, []string{"--acp"}, fixture.newLaunchWorkspace(t), "")
	if err != nil {
		t.Fatalf("start the induction session: %v", err)
	}
	if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "report",
		OnEvent: func(domain.AgentEvent) {},
	}); err != nil {
		t.Fatalf("run one turn against the environment-reporting agent: %v", err)
	}

	reported := environmentReport(t, reportPath)
	if _, carried := reported[ambientEnvName]; carried {
		t.Errorf("the protocol launch carries %s, want it absent: the native surface never sees it, so the two surfaces would answer under different conditions", ambientEnvName)
	}
	if got := reported[configRootEnvName]; got != configRoot {
		t.Errorf("the protocol launch carries %s=%q, want %q: the isolated root the profile names must reach the runtime", configRootEnvName, got, configRoot)
	}
	if got := reported[authEnvName]; got != credential {
		t.Errorf("the protocol launch carries %s=%q, want the credential the coordinates name", authEnvName, got)
	}
}

func TestLaunchEnvWrapperCarriesNoValue(t *testing.T) {
	_, credential := plantEnvironment(t)

	dir := t.TempDir()
	coords := envSurfaceCoordinates("")
	wrapper := writeLaunchEnvWrapper(t, dir, launchEnvNames(coords), "")

	raw, err := os.ReadFile(wrapper) //nolint:gosec // wrapper is this test's own temporary directory
	if err != nil {
		t.Fatalf("read the launch wrapper %q: %v", wrapper, err)
	}
	if !strings.Contains(string(raw), authEnvName) {
		t.Errorf("the launch wrapper does not name %s, want it carried by name", authEnvName)
	}
	if strings.Contains(string(raw), credential) {
		t.Error("the launch wrapper carries the credential's value, want the name alone: an artifact must not hold a credential")
	}
}

func TestValidateEnvNames(t *testing.T) {
	t.Parallel()

	t.Run("shell names pass", func(t *testing.T) {
		t.Parallel()

		if err := validateEnvNames([]string{"PATH", "HOME", "_UNDERSCORED", "MIXED_9"}); err != nil {
			t.Errorf("validateEnvNames(...) = %v, want nil", err)
		}
	})

	t.Run("a coordinate that is not a name is refused", func(t *testing.T) {
		t.Parallel()

		for _, name := range []string{"PATH; rm -rf /", "$(id)", "", "9LEADING", "WITH SPACE"} {
			if err := validateEnvNames([]string{name}); err == nil {
				t.Errorf("validateEnvNames(%q) = nil, want a refusal: the wrapper would expand it as code", name)
			}
		}
	})
}
