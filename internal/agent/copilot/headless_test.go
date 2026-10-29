package copilot

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/copilot/quota"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const headlessServerScenario = "copilot.headless-server"

type headlessServerParams struct {
	CapturePath string
	Answers     string
}

func init() {
	fakeScenarios[headlessServerScenario] = agenttest.Typed(runHeadlessServerScenario)
}

func runHeadlessServerScenario(args []string, params headlessServerParams) int {
	if os.WriteFile(params.CapturePath, []byte(strings.Join(args, " ")), 0o600) != nil {
		return 1
	}

	br := bufio.NewReader(os.Stdin)
	for line := range strings.Lines(params.Answers) {
		reply, ok := strings.CutPrefix(line, "<< ")
		if !ok {
			continue
		}
		if _, err := jsonrpc.ReadContentLengthFrame(br, quota.MaxAnswerBytes); err != nil {
			return 1
		}
		if jsonrpc.WriteContentLengthFrame(os.Stdout, []byte(strings.TrimSpace(reply))) != nil {
			return 1
		}
	}
	return 0
}

func TestCopilotPull_ReachesHeadlessRouteThroughLaunchTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	capturePath := dir + "/captured.json"
	bin := agenttest.FakeRuntime(t, dir, "copilot", headlessServerScenario, headlessServerParams{CapturePath: capturePath, Answers: string(waittest.File(t, "quota/testdata/headless_real.txt"))})

	readings, err := copilotPull(agentcore.LaunchTarget{Command: bin, Args: []string{"--own-arg"}, WorkspacePath: dir})(waittest.Context(t))

	if err != nil || len(readings) != 1 {
		t.Errorf("copilotPull() = %d readings, error %v, want 1 and nil", len(readings), err)
	}
	if got, want := string(waittest.File(t, capturePath)), "--own-arg --headless --no-auto-update --stdio"; got != want {
		t.Errorf("runtime arguments = %q, want %q", got, want)
	}
}
