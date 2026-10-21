package pi

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// fakeScenarios collects every non-default fake-runtime scenario this
// package's tests register. Platform-specific test files add their own
// entries via init.
var fakeScenarios = map[string]agenttest.Scenario{}

func TestMain(m *testing.M) {
	agenttest.Main(m, fakeScenarios)
}
