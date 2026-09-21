//go:build unix

package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func policyRequestingProfile(runtimeID, toolNameFormat, policyFormat string) profile.RuntimeProfile {
	return profile.RuntimeProfile{
		RuntimeID:        runtimeID,
		ToolNameFormat:   toolNameFormat,
		ToolPolicyFormat: policyFormat,
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol: {Args: []string{"--policy", policyPlaceholder, "--acp"}},
		},
	}
}

func TestWriteToolServerPolicyWritesOnlyForARuntimeThatAsksAndIsUnderstood(t *testing.T) {
	t.Parallel()

	t.Run("a profile that asks for no policy file gets none written", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		p := profile.RuntimeProfile{
			RuntimeID:      "other-runtime",
			ToolNameFormat: "@{server}/{tool}",
			EntryPoints: map[evidence.Surface]profile.EntryPoint{
				evidence.SurfaceProtocol: {Args: []string{"--acp"}},
			},
		}

		if path := writeToolServerPolicy(t, dir, p); path != "" {
			t.Errorf("writeToolServerPolicy(...) = %q, want the empty path: a runtime whose launch asks for no policy file reads none", path)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%q) = %v, want nil", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("writeToolServerPolicy(...) left %d file(s) in the launch directory, want none: a file in one runtime's format is a claim about a runtime that never reads it", len(entries))
		}
	})

	t.Run("a runtime that asks for a policy file in no known format is refused", func(t *testing.T) {
		t.Parallel()

		p := policyRequestingProfile("runtime-with-no-writer", "{tool}@{server}", "format-with-no-writer")

		write, err := toolPolicyWriterFor(p)

		if err == nil {
			t.Fatalf("toolPolicyWriterFor(%q) error = nil, want a refusal: substituting a tool name into another runtime's file format produces a file that runtime cannot read", p.RuntimeID)
		}
		if write != nil {
			t.Error("toolPolicyWriterFor(...) returned a writer alongside its refusal, want none")
		}
	})

	t.Run("the written rule names the tool under the runtime's own format", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := writeToolServerPolicy(t, dir, policyRequestingProfile("sample-runtime", "mcp_{server}_{tool}", profile.ToolPolicyFormatTOMLRuleList))

		raw, err := os.ReadFile(path) //nolint:gosec // path is this test's own temporary directory
		if err != nil {
			t.Fatalf("ReadFile(%q) = %v, want nil", path, err)
		}
		if filepath.Dir(path) != dir {
			t.Errorf("writeToolServerPolicy(...) path = %q, want a file under %q", path, dir)
		}
		want := qualifiedToolName("mcp_{server}_{tool}", toolServerName, probeToolName)
		if !strings.Contains(string(raw), want) {
			t.Errorf("the policy file does not name %q; contents:\n%s", want, raw)
		}
	})
}

func TestRequestsToolPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile profile.RuntimeProfile
		want    bool
	}{
		{
			name:    "a protocol entry point asking for one",
			profile: policyRequestingProfile("sample-runtime", "mcp_{server}_{tool}", profile.ToolPolicyFormatTOMLRuleList),
			want:    true,
		},
		{
			name: "an asking posture alone still asks for one",
			profile: profile.RuntimeProfile{
				EntryPoints: map[evidence.Surface]profile.EntryPoint{
					evidence.SurfaceNativeJSON: {
						Args:       []string{"--prompt", "{prompt}"},
						AskingArgs: []string{"--policy", policyPlaceholder, "--prompt", "{prompt}"},
					},
				},
			},
			want: true,
		},
		{
			name: "no entry point asking for one",
			profile: profile.RuntimeProfile{
				EntryPoints: map[evidence.Surface]profile.EntryPoint{
					evidence.SurfaceProtocol:   {Args: []string{"--acp"}},
					evidence.SurfaceNativeJSON: {Args: []string{"--prompt", "{prompt}"}},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := requestsToolPolicy(tt.profile); got != tt.want {
				t.Errorf("requestsToolPolicy(...) = %t, want %t", got, tt.want)
			}
		})
	}
}
