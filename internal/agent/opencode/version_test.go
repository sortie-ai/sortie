package opencode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestParseRuntimeVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		out         string
		truncated   bool
		wantVersion string
		wantMajor   int
		wantOK      bool
	}{
		{name: "1.x bare version", out: "1.18.32\n", wantVersion: "1.18.32", wantMajor: 1, wantOK: true},
		{name: "2.x opencode bin, v-prefixed", out: "opencode v2.0.18\n", wantVersion: "2.0.18", wantMajor: 2, wantOK: true},
		{name: "2.x opencode2 bin, v-prefixed", out: "opencode2 v2.0.18\n", wantVersion: "2.0.18", wantMajor: 2, wantOK: true},
		{name: "2.x prerelease suffix", out: "opencode v2.1.0-beta.3\n", wantVersion: "2.1.0-beta.3", wantMajor: 2, wantOK: true},
		{name: "2.x trailing build metadata token is ignored", out: "opencode 2.0.18 (abc123)\n", wantVersion: "2.0.18", wantMajor: 2, wantOK: true},
		{name: "leading blank lines are skipped", out: "\n\n1.19.0\n", wantVersion: "1.19.0", wantMajor: 1, wantOK: true},
		{name: "an unsupported major still parses", out: "0.0.0-dev-202609250101\n", wantVersion: "0.0.0-dev-202609250101", wantMajor: 0, wantOK: true},
		{name: "major 3 still parses", out: "3.0.0\n", wantVersion: "3.0.0", wantMajor: 3, wantOK: true},
		{name: "empty output is not ok", out: "", wantOK: false},
		{name: "usage text carries no version", out: "Usage: opencode [options]\n", wantOK: false},
		{name: "a two-component number is not a bare semantic version", out: "1.18\n", wantOK: false},
		{name: "truncated output is never ok", out: "1.18.32\n", truncated: true, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, major, ok := parseRuntimeVersion([]byte(tt.out), tt.truncated)

			if ok != tt.wantOK {
				t.Fatalf("parseRuntimeVersion(%q, truncated=%v) ok = %v, want %v", tt.out, tt.truncated, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if version != tt.wantVersion {
				t.Errorf("parseRuntimeVersion(%q) version = %q, want %q", tt.out, version, tt.wantVersion)
			}
			if major != tt.wantMajor {
				t.Errorf("parseRuntimeVersion(%q) major = %d, want %d", tt.out, major, tt.wantMajor)
			}
		})
	}
}

func TestStartSession_RefusesUnsupportedMajor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		reportedVersion string
		refusedVersion  string
	}{
		{name: "major 1", reportedVersion: "1.18.33", refusedVersion: "1.18.33"},
		{name: "major 0 development build", reportedVersion: "0.0.0-dev-202609250101", refusedVersion: "0.0.0-dev-202609250101"},
		{name: "major 3", reportedVersion: "3.0.0", refusedVersion: "3.0.0"},
		{name: "major 2 starts", reportedVersion: "opencode v2.0.18"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			command := agenttest.FakeRuntime(t, dir, "opencode", agenttest.OutputScenario, agenttest.Output{Version: tt.reportedVersion})
			a, err := NewOpenCodeAdapter()
			if err != nil {
				t.Fatalf("NewOpenCodeAdapter() error = %v", err)
			}

			session, err := a.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: dir,
				AgentConfig:   domain.AgentConfig{Command: command},
			})

			if tt.refusedVersion == "" {
				if err != nil {
					t.Fatalf("StartSession(version %q) error = %v, want nil", tt.reportedVersion, err)
				}
				if session.Internal == nil {
					t.Errorf("StartSession(version %q) session = %+v, want a started session", tt.reportedVersion, session)
				}
				return
			}
			if err == nil {
				t.Fatalf("StartSession(version %q) = session %+v, error nil, want an agent_not_found refusal", tt.reportedVersion, session)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("StartSession(version %q) error = %v (%T), want a *domain.AgentError", tt.reportedVersion, err, err)
			}
			if agentErr.Kind != domain.ErrAgentNotFound {
				t.Errorf("StartSession(version %q) Kind = %q, want %q", tt.reportedVersion, agentErr.Kind, domain.ErrAgentNotFound)
			}
			wantMessage := fmt.Sprintf("OpenCode %s is not supported; install OpenCode 2.x, published on npm as @opencode/cli", tt.refusedVersion)
			if agentErr.Message != wantMessage {
				t.Errorf("StartSession(version %q) Message = %q, want %q", tt.reportedVersion, agentErr.Message, wantMessage)
			}
			if session.Internal != nil {
				t.Errorf("StartSession(version %q) session.Internal = %v, want no session", tt.reportedVersion, session.Internal)
			}
		})
	}
}
