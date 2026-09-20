//go:build unix

package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// unauthenticatedScenario prints its version, answers the handshake, but has
// its provider refuse every turn: the shape a stale or missing credential
// takes, where the binary runs and nothing it is asked to do is answered.
const unauthenticatedScenario = "unauthenticated-runtime"

func runUnauthenticated(args []string, _ struct{}) int {
	if len(args) > 0 && args[0] == "--version" {
		if _, err := fmt.Fprintln(os.Stdout, "stub 1.0"); err != nil {
			return 2
		}
		return 0
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
			code = respond(header.ID, `"result":{"sessionId":"unauthenticated-session"}`)
		case "session/prompt":
			code = respond(header.ID, `"error":{"code":-32000,"message":"request failed: 401 unauthorized"}`)
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
	probeScenarios[unauthenticatedScenario] = agenttest.Typed(runUnauthenticated)
}

func canaryCoordinates(runtimePath string) Coordinates {
	return Coordinates{
		CommandPath: runtimePath,
		Model:       "fixture-model",
		Profile: qualification.RuntimeProfile{
			RuntimeID:   "canary-runtime",
			VersionArgs: []string{"--version"},
			EntryPoints: map[qualification.Surface]qualification.EntryPoint{
				qualification.SurfaceProtocol: {Args: []string{"--acp"}},
			},
		},
	}
}

func TestVersionSuccessIsNotAuthenticationSuccess(t *testing.T) {
	if os.Getenv("PROBE_AUTHENTICATION_CANARY_HELPER_PROCESS") == "1" {
		script := agenttest.FakeRuntime(t, t.TempDir(), "runtime", unauthenticatedScenario, struct{}{})
		coords := canaryCoordinates(script)
		fixture := &sharedFixture{workspaceRoot: t.TempDir(), tracker: &groupTracker{}, usage: &usageTracker{}}

		runVersionCanary(t, coords, fixture)
		runAuthenticationCanary(t, coords, fixture)
		t.Fatal("runAuthenticationCanary() returned for a runtime no provider answered")
		return
	}

	t.Run("the version canary passes for an unauthenticated runtime", func(t *testing.T) {
		script := agenttest.FakeRuntime(t, t.TempDir(), "runtime", unauthenticatedScenario, struct{}{})
		fixture := &sharedFixture{workspaceRoot: t.TempDir(), tracker: &groupTracker{}, usage: &usageTracker{}}

		runVersionCanary(t, canaryCoordinates(script), fixture)
	})

	t.Run("the authentication canary stops the run for the same runtime", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestVersionSuccessIsNotAuthenticationSuccess$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
		cmd.Env = append(os.Environ(), "PROBE_AUTHENTICATION_CANARY_HELPER_PROCESS=1")

		output, err := cmd.CombinedOutput()

		if err == nil {
			t.Fatalf("the helper process exited 0, want a failure: a turn no provider answered was accepted as an authenticated run; output:\n%s", output)
		}
		if !strings.Contains(string(output), "authentication canary") {
			t.Errorf("the helper output does not name the authentication canary; output:\n%s", output)
		}
		if strings.Contains(string(output), "version canary") {
			t.Errorf("the version canary failed too, so this control does not separate the two checks; output:\n%s", output)
		}
	})
}
