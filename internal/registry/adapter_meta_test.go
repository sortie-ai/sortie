package registry_test

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"

	// Trigger adapter init() registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/codex"
	_ "github.com/sortie-ai/sortie/internal/agent/copilot"
	_ "github.com/sortie-ai/sortie/internal/agent/kiro"
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

		// samplePassthrough and wantKey apply only when declaresResumeBlocker
		// is true; a kind that declares nothing has no subject for them.
		tests := []struct {
			name                  string
			kind                  string
			wantCommand           bool
			wantMCPInjection      registry.MCPInjection
			declaresResumeBlocker bool
			samplePassthrough     map[string]any
			wantKey               string
		}{
			{
				name:                  "claude-code requires command, declares MCP injection supported, and declares session_persistence as a resume blocker",
				kind:                  "claude-code",
				wantCommand:           true,
				wantMCPInjection:      registry.MCPInjectionSupported,
				declaresResumeBlocker: true,
				samplePassthrough:     map[string]any{"session_persistence": false},
				wantKey:               "session_persistence",
			},
			{
				name:             "copilot-cli requires command, declares MCP injection supported, and declares no resume blocker",
				kind:             "copilot-cli",
				wantCommand:      true,
				wantMCPInjection: registry.MCPInjectionSupported,
			},
			{
				name:             "codex requires command, declares MCP injection translated, and declares no resume blocker",
				kind:             "codex",
				wantCommand:      true,
				wantMCPInjection: registry.MCPInjectionTranslated,
			},
			{
				name:             "kiro requires command, declares MCP injection unsupported, and declares no resume blocker",
				kind:             "kiro",
				wantCommand:      true,
				wantMCPInjection: registry.MCPInjectionUnsupported,
			},
			{
				name:             "opencode requires command, declares MCP injection translated, and declares no resume blocker",
				kind:             "opencode",
				wantCommand:      true,
				wantMCPInjection: registry.MCPInjectionTranslated,
			},
			{
				name:             "mock requires nothing, declares MCP injection unsupported, and declares no resume blocker",
				kind:             "mock",
				wantMCPInjection: registry.MCPInjectionUnsupported,
			},
		}

		byKind := make(map[string]int, len(tests))
		for i, tt := range tests {
			byKind[tt.kind] = i
		}

		registered := registry.Agents.Kinds()

		// Completeness in both directions: every registered kind has an
		// expectation entry, and every expectation entry names a
		// registered kind. A seventh kind added to this file's blank
		// imports without an entry, or an entry naming a kind that is no
		// longer registered, fails here by name before any field is
		// compared.
		for _, kind := range registered {
			if _, ok := byKind[kind]; !ok {
				t.Errorf("Agents.Kinds() includes %q, which has no expectation entry in this table", kind)
			}
		}
		for _, tt := range tests {
			if !slices.Contains(registered, tt.kind) {
				t.Errorf("expectation entry names kind %q, which Agents.Kinds() does not return", tt.kind)
			}
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
				if meta.MCPInjection != tt.wantMCPInjection {
					t.Errorf("Agents.Meta(%q).MCPInjection = %q, want %q", tt.kind, meta.MCPInjection, tt.wantMCPInjection)
				}

				if !tt.declaresResumeBlocker {
					if meta.SessionResumeBlockedBy != nil {
						t.Errorf("Agents.Meta(%q).SessionResumeBlockedBy = non-nil, want nil: this kind's entry says it declares no resume blocker", tt.kind)
					}
					return
				}
				if meta.SessionResumeBlockedBy == nil {
					t.Fatalf("Agents.Meta(%q).SessionResumeBlockedBy = nil, want non-nil: this kind's entry says it declares a resume blocker", tt.kind)
				}

				if got := meta.SessionResumeBlockedBy(tt.samplePassthrough); got != tt.wantKey {
					t.Errorf("Agents.Meta(%q).SessionResumeBlockedBy(%v) = %q, want %q", tt.kind, tt.samplePassthrough, got, tt.wantKey)
				}
				if got := meta.SessionResumeBlockedBy(nil); got != "" {
					t.Errorf("Agents.Meta(%q).SessionResumeBlockedBy(nil) = %q, want \"\"", tt.kind, got)
				}
				if got := meta.SessionResumeBlockedBy(map[string]any{}); got != "" {
					t.Errorf("Agents.Meta(%q).SessionResumeBlockedBy(map[string]any{}) = %q, want \"\"", tt.kind, got)
				}

				populated := maps.Clone(tt.samplePassthrough)
				before := maps.Clone(populated)
				meta.SessionResumeBlockedBy(populated)
				if len(populated) != len(before) {
					t.Errorf("Agents.Meta(%q).SessionResumeBlockedBy mutated the passthrough map's length: got %d keys, want %d", tt.kind, len(populated), len(before))
				}
				if !maps.Equal(populated, before) {
					t.Errorf("Agents.Meta(%q).SessionResumeBlockedBy mutated the passthrough map's values: got %v, want %v", tt.kind, populated, before)
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

// TestAgentConfigFields_ShapeUnchanged pins registry.AgentConfigFields to
// exactly its two documented fields, Kind and Passthrough, by reflection,
// so a silent widening of the struct fails here rather than only being
// visible in a diff.
func TestAgentConfigFields_ShapeUnchanged(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[registry.AgentConfigFields]()
	if typ.NumField() != 2 {
		names := make([]string, typ.NumField())
		for i := range typ.NumField() {
			names[i] = typ.Field(i).Name
		}
		t.Fatalf("registry.AgentConfigFields has %d fields, want 2: %v", typ.NumField(), names)
	}
	if got := typ.Field(0).Name; got != "Kind" {
		t.Errorf("registry.AgentConfigFields field 0 = %q, want %q", got, "Kind")
	}
	if got := typ.Field(1).Name; got != "Passthrough" {
		t.Errorf("registry.AgentConfigFields field 1 = %q, want %q", got, "Passthrough")
	}
}
