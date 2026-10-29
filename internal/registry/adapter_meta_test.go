package registry_test

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/registry"

	// Trigger adapter init() registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	_ "github.com/sortie-ai/sortie/internal/agent/codex"
	_ "github.com/sortie-ai/sortie/internal/agent/copilot"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/agent/opencode"
	_ "github.com/sortie-ai/sortie/internal/scm/gitea"
	_ "github.com/sortie-ai/sortie/internal/scm/github"
	_ "github.com/sortie-ai/sortie/internal/scm/gitlab"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
	_ "github.com/sortie-ai/sortie/internal/tracker/linear"
)

func assertMCPDialectDeclaration(t *testing.T, kind string, meta registry.AgentMeta) {
	t.Helper()

	if meta.MCPDialect == registry.MCPDialectUndeclared {
		t.Errorf("Agents.Meta(%q).MCPDialect = %q, want a declared dialect", kind, meta.MCPDialect)
		return
	}

	isNone := meta.MCPDialect == registry.MCPDialectNone
	if isNone && meta.MCPDialectNoneReason == "" {
		t.Errorf("Agents.Meta(%q).MCPDialectNoneReason = %q, want non-empty with MCPDialectNone", kind, meta.MCPDialectNoneReason)
	}
	if !isNone && meta.MCPDialectNoneReason != "" {
		t.Errorf("Agents.Meta(%q).MCPDialectNoneReason = %q, want empty with dialect %q", kind, meta.MCPDialectNoneReason, meta.MCPDialect)
	}
	if got := meta.MCPDialect.DeliversTools(); got == isNone {
		t.Errorf("Agents.Meta(%q).MCPDialect.DeliversTools() = %v, want %v", kind, got, !isNone)
	}

	_, err := mcpconfig.Config{}.Render(meta.MCPDialect, mcpconfig.RenderInput{})
	if isNone {
		if !errors.Is(err, mcpconfig.ErrUnknownDialect) {
			t.Errorf("Config.Render(%q) error = %v, want ErrUnknownDialect", meta.MCPDialect, err)
		}
		return
	}
	if err != nil {
		t.Errorf("Config.Render(%q) = error %v, want nil", meta.MCPDialect, err)
	}
}

func usageLimitDeclarationProblem(meta registry.AgentMeta) string {
	hasReason := meta.UsageLimitNotRecognizedReason != ""

	switch meta.UsageLimitRecognition {
	case registry.UsageLimitNotRecognized:
		if !hasReason {
			return "not_recognized without a reason"
		}
	case registry.UsageLimitRecognized:
		if hasReason {
			return "recognized with a reason"
		}
	case registry.UsageLimitRecognitionUndeclared:
		return "undeclared"
	default:
		return "unknown value " + string(meta.UsageLimitRecognition)
	}
	return ""
}

func TestUsageLimitDeclarationProblem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta registry.AgentMeta
		want bool
	}{
		{"zero value", registry.AgentMeta{}, true},
		{"not_recognized without a reason", registry.AgentMeta{UsageLimitRecognition: registry.UsageLimitNotRecognized}, true},
		{
			"recognized with a reason",
			registry.AgentMeta{UsageLimitRecognition: registry.UsageLimitRecognized, UsageLimitNotRecognizedReason: "stale"},
			true,
		},
		{"recognized", registry.AgentMeta{UsageLimitRecognition: registry.UsageLimitRecognized}, false},
		{
			"not_recognized with a reason",
			registry.AgentMeta{UsageLimitRecognition: registry.UsageLimitNotRecognized, UsageLimitNotRecognizedReason: "no form"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := usageLimitDeclarationProblem(tt.meta) != ""

			if got != tt.want {
				t.Errorf("usageLimitDeclarationProblem(%+v) non-empty = %v, want %v", tt.meta, got, tt.want)
			}
		})
	}
}

func TestAdapterMeta_RealRegistrations(t *testing.T) {
	t.Parallel()

	t.Run("tracker adapters", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name               string
			kind               string
			wantAPIKey         bool
			wantProject        bool
			wantTerminalStates bool
		}{
			{
				name:        "jira requires api_key and project",
				kind:        "jira",
				wantAPIKey:  true,
				wantProject: true,
			},
			{
				name:               "github requires api_key, project and terminal states",
				kind:               "github",
				wantAPIKey:         true,
				wantProject:        true,
				wantTerminalStates: true,
			},
			{
				name:               "gitea requires api_key, project and terminal states",
				kind:               "gitea",
				wantAPIKey:         true,
				wantProject:        true,
				wantTerminalStates: true,
			},
			{
				name:               "gitlab requires api_key, project and terminal states",
				kind:               "gitlab",
				wantAPIKey:         true,
				wantProject:        true,
				wantTerminalStates: true,
			},
			{
				name:        "linear requires api_key and project",
				kind:        "linear",
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
				if meta.RequiresTerminalStates != tt.wantTerminalStates {
					t.Errorf("Trackers.Meta(%q).RequiresTerminalStates = %v, want %v", tt.kind, meta.RequiresTerminalStates, tt.wantTerminalStates)
				}
			})
		}
	})

	t.Run("agent adapters", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name                   string
			kind                   string
			wantCommand            bool
			wantMCPDialect         registry.MCPDialect
			wantUsageArrival       registry.UsageArrival
			wantUsageAttribution   registry.UsageAttribution
			declaresResumeBlocker  bool
			samplePassthrough      map[string]any
			wantKey                string
			wantCredentialEnvNames []string
			wantEffortForwarding   registry.EffortForwarding
			wantUsageLimit         registry.UsageLimitRecognition
		}{
			{
				name:                   "agent-client-protocol requires command, declares MCP dialect acp_session, derives turn_end/session_total usage, declares no resume blocker, and declares no credential names",
				kind:                   "agent-client-protocol",
				wantCommand:            true,
				wantMCPDialect:         registry.MCPDialectACPSession,
				wantUsageArrival:       registry.UsageArrivalTurnEnd,
				wantUsageAttribution:   registry.UsageAttributionSessionTotal,
				wantCredentialEnvNames: nil,
				wantEffortForwarding:   registry.EffortNotForwarded,
				wantUsageLimit:         registry.UsageLimitNotRecognized,
			},
			{
				name:                   "claude-code requires command, declares MCP dialect mcp_servers_json_always_load, derives incremental/per_model usage, declares session_persistence as a resume blocker, and declares its Anthropic credential names",
				kind:                   "claude-code",
				wantCommand:            true,
				wantMCPDialect:         registry.MCPDialectMCPServersJSONAlwaysLoad,
				wantUsageArrival:       registry.UsageArrivalIncremental,
				wantUsageAttribution:   registry.UsageAttributionPerModel,
				declaresResumeBlocker:  true,
				samplePassthrough:      map[string]any{"session_persistence": false},
				wantKey:                "session_persistence",
				wantCredentialEnvNames: []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
				wantEffortForwarding:   registry.EffortForwarded,
				wantUsageLimit:         registry.UsageLimitRecognized,
			},
			{
				name:                   "copilot-cli requires command, declares MCP dialect mcp_servers_json, derives turn_end/per_model usage, declares no resume blocker, and declares its GitHub-token credential names",
				kind:                   "copilot-cli",
				wantCommand:            true,
				wantMCPDialect:         registry.MCPDialectMCPServersJSON,
				wantUsageArrival:       registry.UsageArrivalTurnEnd,
				wantUsageAttribution:   registry.UsageAttributionPerModel,
				wantCredentialEnvNames: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
				wantEffortForwarding:   registry.EffortForwarded,
				wantUsageLimit:         registry.UsageLimitNotRecognized,
			},
			{
				name:                   "codex requires command, declares MCP dialect mcp_servers_toml, derives incremental/per_model usage, declares no resume blocker, and declares no credential names",
				kind:                   "codex",
				wantCommand:            true,
				wantMCPDialect:         registry.MCPDialectMCPServersTOML,
				wantUsageArrival:       registry.UsageArrivalIncremental,
				wantUsageAttribution:   registry.UsageAttributionPerModel,
				wantCredentialEnvNames: nil,
				wantEffortForwarding:   registry.EffortForwarded,
				wantUsageLimit:         registry.UsageLimitRecognized,
			},
			{
				name:                   "opencode requires command, declares MCP dialect mcp_member_json, derives turn_end/per_model usage, declares no resume blocker, and declares no credential names",
				kind:                   "opencode",
				wantCommand:            true,
				wantMCPDialect:         registry.MCPDialectMCPMemberJSON,
				wantUsageArrival:       registry.UsageArrivalTurnEnd,
				wantUsageAttribution:   registry.UsageAttributionPerModel,
				wantCredentialEnvNames: nil,
				wantEffortForwarding:   registry.EffortForwarded,
				wantUsageLimit:         registry.UsageLimitNotRecognized,
			},
			{
				name:                   "mock requires nothing, declares MCP dialect none, derives incremental/session_total usage, declares no resume blocker, and declares no credential names",
				kind:                   "mock",
				wantMCPDialect:         registry.MCPDialectNone,
				wantUsageArrival:       registry.UsageArrivalIncremental,
				wantUsageAttribution:   registry.UsageAttributionSessionTotal,
				wantCredentialEnvNames: nil,
				wantEffortForwarding:   registry.EffortNotForwarded,
				wantUsageLimit:         registry.UsageLimitRecognized,
			},
		}

		byKind := make(map[string]int, len(tests))
		for i, tt := range tests {
			byKind[tt.kind] = i
		}

		registered := registry.Agents.Kinds()

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
				if meta.MCPDialect != tt.wantMCPDialect {
					t.Errorf("Agents.Meta(%q).MCPDialect = %q, want %q", tt.kind, meta.MCPDialect, tt.wantMCPDialect)
				}
				assertMCPDialectDeclaration(t, tt.kind, meta)
				if err := meta.UsageSources.Check(); err != nil {
					t.Errorf("Agents.Meta(%q).UsageSources.Check() = %v, want nil", tt.kind, err)
				}
				arrival, attribution := meta.UsageDisposition()
				if arrival != tt.wantUsageArrival || attribution != tt.wantUsageAttribution {
					t.Errorf("Agents.Meta(%q).UsageDisposition() = %q, %q, want %q, %q", tt.kind, arrival, attribution, tt.wantUsageArrival, tt.wantUsageAttribution)
				}

				if !meta.CredentialEnv.Declared() {
					t.Errorf("Agents.Meta(%q).CredentialEnv.Declared() = false, want true: every registered kind declares its credential names", tt.kind)
				}
				if got := meta.CredentialEnv.Names(); !slices.Equal(got, tt.wantCredentialEnvNames) {
					t.Errorf("Agents.Meta(%q).CredentialEnv.Names() = %v, want %v", tt.kind, got, tt.wantCredentialEnvNames)
				}

				if meta.EffortForwarding != tt.wantEffortForwarding {
					t.Errorf("Agents.Meta(%q).EffortForwarding = %q, want %q", tt.kind, meta.EffortForwarding, tt.wantEffortForwarding)
				}

				if meta.UsageLimitRecognition != tt.wantUsageLimit {
					t.Errorf("Agents.Meta(%q).UsageLimitRecognition = %q, want %q", tt.kind, meta.UsageLimitRecognition, tt.wantUsageLimit)
				}
				if problem := usageLimitDeclarationProblem(meta); problem != "" {
					t.Errorf("Agents.Meta(%q) usage-limit declaration is invalid: %s", tt.kind, problem)
				}

				if meta.Deprecation != nil {
					t.Errorf("Agents.Meta(%q).Deprecation = %+v, want nil", tt.kind, meta.Deprecation)
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
