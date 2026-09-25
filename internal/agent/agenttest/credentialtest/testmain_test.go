package credentialtest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestMain(m *testing.M) {
	agenttest.Main(m, map[string]agenttest.Scenario{})
}
