package pi_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	_ "github.com/sortie-ai/sortie/internal/agent/pi"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// scriptedModel is a built-in anthropic model, so pi accepts it without a
// models.json entry of its own.
const scriptedModel = "claude-sonnet-4-5"

func TestIntegration_ScriptedModel(t *testing.T) {
	if os.Getenv("SORTIE_PI_TEST") != "1" {
		t.Skip("set SORTIE_PI_TEST=1 to run pi integration tests")
	}

	fakemodel.AssertConformance(t, fakemodel.Binding{
		Kind:                "pi",
		Passthrough:         map[string]any{"model": "anthropic/" + scriptedModel},
		CredentialEnv:       []string{"ANTHROPIC_API_KEY"},
		ReportedModelPrefix: "anthropic/",
		UserInstructions:    filepath.Join("pi-agent", "AGENTS.md"),
		Spend: &fakemodel.SpendSources{
			Live: domain.UsageProvenance{Source: usage.SourceOutput, Strategy: usage.StrategyLive},
		},
		Read: func(path string) fakemodel.ToolChoice {
			arguments, _ := json.Marshal(map[string]string{"path": path})
			return fakemodel.Named("read", arguments)
		},
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			agentDir := filepath.Join(env.Home, "pi-agent")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatalf("create %s: %v", agentDir, err)
			}
			models := `{"providers":{"anthropic":{"baseUrl":"` + env.URL + `"}}}`
			if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
				t.Fatalf("write models.json: %v", err)
			}
			return fakemodel.Launch{
				Config: domain.AgentConfig{
					Command:       "pi",
					TurnTimeoutMS: 300000,
					ReadTimeoutMS: 60000,
				},
				Env: map[string]string{
					"PI_CODING_AGENT_DIR": agentDir,
					"PI_OFFLINE":          "1",
				},
			}
		},
	})
}
