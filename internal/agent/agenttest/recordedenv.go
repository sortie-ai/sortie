package agenttest

import (
	"encoding/json"
	"fmt"
	"os"
)

// RecordedEnvScenario names the built-in [Scenario] that runs
// [RecordedEnv.Run].
const RecordedEnvScenario = "agenttest.recordedenv"

// RecordedEnv parameterizes [RecordedEnvScenario].
type RecordedEnv struct {
	Path  string
	Names []string
}

// Run publishes the current value of each name in Names to Path as a
// JSON object, then blocks until the process is killed. Path appears
// atomically, so a reader polling it never observes a partial write.
func (p RecordedEnv) Run() int {
	values := make(map[string]string, len(p.Names))
	for _, name := range p.Names {
		values[name] = os.Getenv(name)
	}
	data, err := json.Marshal(values)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: encode recorded environment: %v\n", err)
		return 2
	}
	tmp := p.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: write recorded environment: %v\n", err)
		return 2
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: publish recorded environment: %v\n", err)
		return 2
	}
	Hang()
	return 0
}

func runRecordedEnv(_ []string, params RecordedEnv) int {
	return params.Run()
}
