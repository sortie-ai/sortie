package pi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestStartSession_RefusesOldVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		reportedVersion string
		refusedVersion  string
	}{
		{name: "previous npm line", reportedVersion: "0.73.1", refusedVersion: "0.73.1"},
		{name: "just below the floor", reportedVersion: "0.98.9", refusedVersion: "0.98.9"},
		{name: "floor starts", reportedVersion: "0.99.0"},
		{name: "newer line starts", reportedVersion: "1.4.0"},
		{name: "v-prefixed floor starts", reportedVersion: "v0.99.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			command := agenttest.FakeRuntime(t, dir, "pi", agenttest.OutputScenario, agenttest.Output{Version: tt.reportedVersion})
			a, err := NewPiAdapter()
			if err != nil {
				t.Fatalf("NewPiAdapter() error = %v", err)
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
			wantMessage := fmt.Sprintf("Pi %s is not supported; install Pi 0.99.0 or later, published on npm as @earendil-works/pi-coding-agent", tt.refusedVersion)
			if agentErr.Message != wantMessage {
				t.Errorf("StartSession(version %q) Message = %q, want %q", tt.reportedVersion, agentErr.Message, wantMessage)
			}
			if session.Internal != nil {
				t.Errorf("StartSession(version %q) session.Internal = %v, want no session", tt.reportedVersion, session.Internal)
			}
		})
	}
}

func TestStartSession_RefusesUnreadableVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	command := agenttest.FakeRuntime(t, dir, "pi", agenttest.OutputScenario, agenttest.Output{Version: "Usage: pi [options]"})
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}

	_, err = a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: command},
	})

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("StartSession() error = %v (%T), want a *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrAgentNotFound {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrAgentNotFound)
	}
	const want = "the configured Pi command reported no version Sortie can read: Usage: pi [options]"
	if agentErr.Message != want {
		t.Errorf("Message = %q, want %q", agentErr.Message, want)
	}
}
