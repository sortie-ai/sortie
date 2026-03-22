package registry_test

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"

	// Trigger adapter init() registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
)

func TestAdapterMeta_RealRegistrations(t *testing.T) {
	t.Parallel()

	t.Run("tracker adapters", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			kind        string
			wantAPIKey  bool
			wantProject bool
			wantCommand bool
		}{
			{
				name:        "jira requires api_key and project",
				kind:        "jira",
				wantAPIKey:  true,
				wantProject: true,
			},
			{
				name: "file requires neither api_key nor project",
				kind: "file",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				meta := registry.Trackers.Meta(tt.kind)

				if meta.RequiresAPIKey != tt.wantAPIKey {
					t.Errorf("Trackers.Meta(%q).RequiresAPIKey = %v, want %v", tt.kind, meta.RequiresAPIKey, tt.wantAPIKey)
				}
				if meta.RequiresProject != tt.wantProject {
					t.Errorf("Trackers.Meta(%q).RequiresProject = %v, want %v", tt.kind, meta.RequiresProject, tt.wantProject)
				}
				if meta.RequiresCommand != tt.wantCommand {
					t.Errorf("Trackers.Meta(%q).RequiresCommand = %v, want %v", tt.kind, meta.RequiresCommand, tt.wantCommand)
				}
			})
		}
	})

	t.Run("agent adapters", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			kind        string
			wantAPIKey  bool
			wantProject bool
			wantCommand bool
		}{
			{
				name:        "claude-code requires command",
				kind:        "claude-code",
				wantCommand: true,
			},
			{
				name: "mock requires nothing",
				kind: "mock",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				meta := registry.Agents.Meta(tt.kind)

				if meta.RequiresAPIKey != tt.wantAPIKey {
					t.Errorf("Agents.Meta(%q).RequiresAPIKey = %v, want %v", tt.kind, meta.RequiresAPIKey, tt.wantAPIKey)
				}
				if meta.RequiresProject != tt.wantProject {
					t.Errorf("Agents.Meta(%q).RequiresProject = %v, want %v", tt.kind, meta.RequiresProject, tt.wantProject)
				}
				if meta.RequiresCommand != tt.wantCommand {
					t.Errorf("Agents.Meta(%q).RequiresCommand = %v, want %v", tt.kind, meta.RequiresCommand, tt.wantCommand)
				}
			})
		}
	})
}
