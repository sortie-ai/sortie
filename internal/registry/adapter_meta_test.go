package registry_test

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"

	// Trigger adapter init() registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/agent/opencode"
	_ "github.com/sortie-ai/sortie/internal/scm/gitea"
	_ "github.com/sortie-ai/sortie/internal/scm/github"
	_ "github.com/sortie-ai/sortie/internal/scm/gitlab"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
	_ "github.com/sortie-ai/sortie/internal/tracker/linear"
)

func TestAdapterMeta_RealRegistrations(t *testing.T) {
	t.Parallel()

	t.Run("tracker adapters", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name         string
			kind         string
			wantAPIKey   bool
			wantProject  bool
			wantActive   []string
			wantTerminal []string
		}{
			{
				name:        "jira requires api_key and project and declares active states only",
				kind:        "jira",
				wantAPIKey:  true,
				wantProject: true,
				wantActive:  []string{"Backlog", "Selected for Development", "In Progress"},
			},
			{
				name:         "github requires api_key and project and declares both state lists",
				kind:         "github",
				wantAPIKey:   true,
				wantProject:  true,
				wantActive:   []string{"backlog", "in-progress", "review"},
				wantTerminal: []string{"done", "wontfix"},
			},
			{
				name:         "gitea requires api_key and project and declares both state lists",
				kind:         "gitea",
				wantAPIKey:   true,
				wantProject:  true,
				wantActive:   []string{"backlog", "in-progress", "review"},
				wantTerminal: []string{"done", "wontfix"},
			},
			{
				name:         "gitlab requires api_key and project and declares both state lists",
				kind:         "gitlab",
				wantAPIKey:   true,
				wantProject:  true,
				wantActive:   []string{"backlog", "in-progress", "review"},
				wantTerminal: []string{"done", "wontfix"},
			},
			{
				name:         "linear requires api_key and project and declares both state lists",
				kind:         "linear",
				wantAPIKey:   true,
				wantProject:  true,
				wantActive:   []string{"Backlog", "Todo", "In Progress"},
				wantTerminal: []string{"Done", "Canceled", "Duplicate"},
			},
			{
				name: "file requires neither api_key nor project and declares no default states",
				kind: "file",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				meta, ok := registry.Trackers.Meta(tt.kind)
				if !ok {
					t.Fatalf("Trackers.Meta(%q) reported not registered", tt.kind)
				}

				if meta.RequiresAPIKey != tt.wantAPIKey {
					t.Errorf("Trackers.Meta(%q).RequiresAPIKey = %v, want %v", tt.kind, meta.RequiresAPIKey, tt.wantAPIKey)
				}
				if meta.RequiresProject != tt.wantProject {
					t.Errorf("Trackers.Meta(%q).RequiresProject = %v, want %v", tt.kind, meta.RequiresProject, tt.wantProject)
				}
				if !slices.Equal(meta.DefaultActiveStates, tt.wantActive) {
					t.Errorf("Trackers.Meta(%q).DefaultActiveStates = %v, want %v", tt.kind, meta.DefaultActiveStates, tt.wantActive)
				}
				if !slices.Equal(meta.DefaultTerminalStates, tt.wantTerminal) {
					t.Errorf("Trackers.Meta(%q).DefaultTerminalStates = %v, want %v", tt.kind, meta.DefaultTerminalStates, tt.wantTerminal)
				}
			})
		}
	})

	t.Run("agent adapters", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			kind        string
			wantCommand bool
		}{
			{
				name:        "claude-code requires command",
				kind:        "claude-code",
				wantCommand: true,
			},
			{
				name:        "opencode requires command",
				kind:        "opencode",
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

				meta, ok := registry.Agents.Meta(tt.kind)
				if !ok {
					t.Fatalf("Agents.Meta(%q) reported not registered", tt.kind)
				}

				if meta.RequiresCommand != tt.wantCommand {
					t.Errorf("Agents.Meta(%q).RequiresCommand = %v, want %v", tt.kind, meta.RequiresCommand, tt.wantCommand)
				}
			})
		}
	})

	t.Run("github exposes ValidateTrackerConfig", func(t *testing.T) {
		t.Parallel()

		meta, ok := registry.Trackers.Meta("github")
		if !ok {
			t.Fatal(`Trackers.Meta("github") reported not registered`)
		}
		if meta.ValidateTrackerConfig == nil {
			t.Error(`Trackers.Meta("github").ValidateTrackerConfig = nil, want non-nil`)
		}
	})

	t.Run("gitlab exposes ValidateTrackerConfig", func(t *testing.T) {
		t.Parallel()

		meta, ok := registry.Trackers.Meta("gitlab")
		if !ok {
			t.Fatal(`Trackers.Meta("gitlab") reported not registered`)
		}
		if meta.ValidateTrackerConfig == nil {
			t.Error(`Trackers.Meta("gitlab").ValidateTrackerConfig = nil, want non-nil`)
		}
	})
}
