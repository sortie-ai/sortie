//go:build unix

package probe

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
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

	path := writeToolServerPolicy(t, dir, policyRequestingProfile("sample-runtime", toolNameFormat, profile.ToolPolicyFormatTOMLRuleList))

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

func TestDeclareToolServerFollowsAdvertisedHTTPSupport(t *testing.T) {
	t.Parallel()

	traces := map[mcpconfig.Transport]string{
		mcpconfig.TransportHTTP:  `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}`,
		mcpconfig.TransportStdio: "",
	}
	for want, trace := range traces {
		t.Run(string(want), func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeWireTrace(t, dir, trace)
			recordPath := filepath.Join(dir, "calls.jsonl")
			servers, err := mcpconfig.Parse((&sharedFixture{wireTraceDir: dir}).declareToolServer(t, dir, recordPath))
			if err != nil || len(servers) != 1 || servers[0].Transport != want {
				t.Fatalf("declareToolServer() declared %+v (parse error %v), want one %q server", servers, err, want)
			}
			if want != mcpconfig.TransportHTTP {
				return
			}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, servers[0].URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			if recorded, _ := fileHasContent(recordPath); !recorded {
				t.Errorf("POST %q = %v, but no tools/call reached %q, want it recorded", servers[0].URL, err, recordPath)
			}
		})
	}
}
