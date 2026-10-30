package usagesource

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

func TestNamesRuntimeMatchesTheGeminiCommandLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		args    string
		want    bool
	}{
		{"bare executable", "/usr/local/bin/gemini", "--acp", true},
		{"windows launcher suffix, forward slashes", "C:/Users/u/AppData/Roaming/npm/gemini.cmd", "--acp", true},
		{"windows launcher suffix, backslashes", `C:\Users\u\AppData\Roaming\npm\gemini.cmd`, "--acp", true},
		{"versioned npx package", "/usr/bin/npx", "-y @google/gemini-cli@0.61.0 --acp", true},
		{"node_modules path", "/usr/bin/node", "/usr/lib/node_modules/@google/gemini-cli/dist/index.js --acp", true},
		{"bundle path as an option value", "/usr/bin/node", "--no-warnings /opt/gemini-cli/bundle/gemini.js --acp", true},
		{"wrapper script naming it bare", "/tmp/t/capture.sh", "gemini --acp", true},
		{"preceded by an inline assignment", "/usr/bin/env", "GEMINI_CLI_HOME=/h gemini --acp", true},
		{"after a double-dash separator", "/usr/bin/mise", "exec -- gemini --acp", true},
		{"a path option value naming another flag", "/usr/local/bin/copilot", "--acp --add-dir /home/u/src/gemini", true},
		{"a bare option value with no path separator", "/usr/local/bin/wrapper", "--verbose gemini --acp", false},
		{"a short-flag option value", "/usr/local/bin/kiro-cli", "acp -a gemini", false},
		{"a model flag's bare option value", "/usr/local/bin/opencode", "acp --model gemini", false},
		{"a model flag naming a path-like model id", "/usr/local/bin/opencode", "acp --model google/gemini-2.5-pro", false},
		{"a model flag naming a version-like model id", "/usr/local/bin/copilot", "--acp --model gemini-3-pro-preview", false},
		{"a short flag with no value at all", "/usr/local/bin/kiro-cli", "acp -a", false},
		{"an unrelated scoped package", "/usr/bin/npx", "@zed-industries/claude-code-acp", false},
		{"a differently named scoped package", "/usr/bin/npx", "@google/gemini-cli-core", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := agentcore.LaunchTarget{Command: tt.command, Args: strings.Fields(tt.args)}

			if got := namesRuntime(target, geminiExecutableName, geminiPackageName); got != tt.want {
				t.Errorf("namesRuntime(%+v) = %v, want %v", target, got, tt.want)
			}
		})
	}
}
