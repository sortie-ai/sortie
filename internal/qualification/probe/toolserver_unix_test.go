//go:build unix

package probe

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

type toolPolicyRule struct {
	ToolName string
	Decision string
}

var (
	toolPolicyRuleHeader = regexp.MustCompile(`(?m)^\[\[rule\]\]\s*$`)
	toolPolicyToolName   = regexp.MustCompile(`(?m)^toolName\s*=\s*"([^"]*)"\s*$`)
	toolPolicyDecision   = regexp.MustCompile(`(?m)^decision\s*=\s*"([^"]*)"\s*$`)
)

func parseToolPolicyRules(t *testing.T, content string) []toolPolicyRule {
	t.Helper()

	headers := toolPolicyRuleHeader.FindAllStringIndex(content, -1)
	rules := make([]toolPolicyRule, 0, len(headers))
	for i, header := range headers {
		start := header[1]
		end := len(content)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
		block := content[start:end]

		var rule toolPolicyRule
		if m := toolPolicyToolName.FindStringSubmatch(block); m != nil {
			rule.ToolName = m[1]
		}
		if m := toolPolicyDecision.FindStringSubmatch(block); m != nil {
			rule.Decision = m[1]
		}
		rules = append(rules, rule)
	}
	return rules
}

func TestQualifiedToolName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		toolNameFormat string
		server         string
		tool           string
		want           string
	}{
		{
			name:           "the runtime's own placeholder order is honored",
			toolNameFormat: "mcp_{server}_{tool}",
			server:         "sortie-probe-tools",
			tool:           "sortie_probe_tool",
			want:           "mcp_sortie-probe-tools_sortie_probe_tool",
		},
		{
			name:           "a different format produces a different qualified name from the same inputs",
			toolNameFormat: "{tool}@{server}",
			server:         "sortie-probe-tools",
			tool:           "sortie_probe_tool",
			want:           "sortie_probe_tool@sortie-probe-tools",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := qualifiedToolName(tt.toolNameFormat, tt.server, tt.tool)

			if got != tt.want {
				t.Errorf("qualifiedToolName(%q, %q, %q) = %q, want %q", tt.toolNameFormat, tt.server, tt.tool, got, tt.want)
			}
		})
	}
}

func TestWriteToolServerPolicy(t *testing.T) {
	t.Parallel()

	const toolNameFormat = "mcp_{server}_{tool}"
	dir := t.TempDir()

	path := writeToolServerPolicy(t, dir, policyRequestingProfile("sample-runtime", toolNameFormat, qualification.ToolPolicyFormatTOMLRuleList))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v, want nil", path, err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("writeToolServerPolicy(%q, %q) path = %q, want a file under %q", dir, toolNameFormat, path, dir)
	}

	rules := parseToolPolicyRules(t, string(raw))
	if len(rules) != 1 {
		t.Fatalf("writeToolServerPolicy(%q, %q) wrote %d rule(s), want exactly 1: %+v", dir, toolNameFormat, len(rules), rules)
	}

	wantToolName := qualifiedToolName(toolNameFormat, toolServerName, probeToolName)
	if rules[0].ToolName != wantToolName {
		t.Errorf("writeToolServerPolicy(%q, %q) rule toolName = %q, want %q", dir, toolNameFormat, rules[0].ToolName, wantToolName)
	}
	if rules[0].Decision != "allow" {
		t.Errorf("writeToolServerPolicy(%q, %q) rule decision = %q, want %q", dir, toolNameFormat, rules[0].Decision, "allow")
	}
}
