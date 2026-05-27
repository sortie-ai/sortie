package kiro

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// assertHasArgPair fails if flag and value do not appear as consecutive
// elements in args.
func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := range len(args) - 1 {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

// assertHasToken fails if token does not appear anywhere in args.
func assertHasToken(t *testing.T, args []string, token string) {
	t.Helper()
	if !slices.Contains(args, token) {
		t.Errorf("buildArgs() missing token %q in [%s]", token, strings.Join(args, " "))
	}
}

// assertNoToken fails if token appears anywhere in args.
func assertNoToken(t *testing.T, args []string, token string) {
	t.Helper()
	if slices.Contains(args, token) {
		t.Errorf("buildArgs() unexpectedly contains token %q in [%s]", token, strings.Join(args, " "))
	}
}

// countToken returns how many times token appears in args.
func countToken(args []string, token string) int {
	n := 0
	for _, a := range args {
		if a == token {
			n++
		}
	}
	return n
}

func TestNewKiroAdapter_TrustModeConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name: "trust_all_tools with non-empty trust_tools conflicts",
			config: map[string]any{
				"trust_all_tools": true,
				"trust_tools":     []any{"read", "grep"},
			},
			wantErr: true,
		},
		{
			name:    "trust_all_tools alone is valid",
			config:  map[string]any{"trust_all_tools": true},
			wantErr: false,
		},
		{
			name:    "trust_tools alone is valid",
			config:  map[string]any{"trust_tools": []any{"read", "grep"}},
			wantErr: false,
		},
		{
			name: "trust_all_tools with empty trust_tools is valid",
			config: map[string]any{
				"trust_all_tools": true,
				"trust_tools":     []any{},
			},
			wantErr: false,
		},
		{
			name:    "empty config is valid",
			config:  map[string]any{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter, err := NewKiroAdapter(tt.config)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewKiroAdapter(%v) error = nil, want error", tt.config)
				}
				if adapter != nil {
					t.Errorf("NewKiroAdapter(%v) adapter = %v, want nil on conflict", tt.config, adapter)
				}
				if !strings.Contains(err.Error(), "trust_all_tools") || !strings.Contains(err.Error(), "trust_tools") {
					t.Errorf("NewKiroAdapter(%v) error = %q, want it to name both trust_all_tools and trust_tools", tt.config, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("NewKiroAdapter(%v) error = %v, want nil", tt.config, err)
			}
			if adapter == nil {
				t.Fatalf("NewKiroAdapter(%v) adapter = nil, want non-nil", tt.config)
			}
		})
	}
}

func TestParsePassthroughConfig_Fields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		want   passthroughConfig
	}{
		{
			name:   "empty config yields zero-value defaults",
			config: map[string]any{},
			want:   passthroughConfig{},
		},
		{
			name:   "nil config yields zero-value defaults",
			config: nil,
			want:   passthroughConfig{},
		},
		{
			name: "allowlist fields extracted",
			config: map[string]any{
				"model":       "claude-sonnet-4.6",
				"trust_tools": []any{"read", "grep", "glob"},
				"agent":       "my-agent",
			},
			want: passthroughConfig{
				Model:      "claude-sonnet-4.6",
				TrustTools: []string{"read", "grep", "glob"},
				Agent:      "my-agent",
			},
		},
		{
			name:   "trust_all_tools flag extracted",
			config: map[string]any{"trust_all_tools": true},
			want:   passthroughConfig{TrustAllTools: true},
		},
		{
			name:   "only model set",
			config: map[string]any{"model": "claude-opus-4.7"},
			want:   passthroughConfig{Model: "claude-opus-4.7"},
		},
		{
			name:   "wrong-typed model takes zero-value default",
			config: map[string]any{"model": 42},
			want:   passthroughConfig{},
		},
		{
			name:   "wrong-typed trust_all_tools takes false default",
			config: map[string]any{"trust_all_tools": "yes"},
			want:   passthroughConfig{},
		},
		{
			name:   "wrong-typed trust_tools yields nil slice",
			config: map[string]any{"trust_tools": "read,grep"},
			want:   passthroughConfig{},
		},
		{
			name:   "non-string trust_tools elements are skipped",
			config: map[string]any{"trust_tools": []any{"read", 7, "grep"}},
			want:   passthroughConfig{TrustTools: []string{"read", "grep"}},
		},
		{
			name:   "wrong-typed agent takes zero-value default",
			config: map[string]any{"agent": true},
			want:   passthroughConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parsePassthroughConfig(tt.config)
			if err != nil {
				t.Fatalf("parsePassthroughConfig(%v) error = %v, want nil", tt.config, err)
			}

			if got.Model != tt.want.Model {
				t.Errorf("parsePassthroughConfig(%v).Model = %q, want %q", tt.config, got.Model, tt.want.Model)
			}
			if got.TrustAllTools != tt.want.TrustAllTools {
				t.Errorf("parsePassthroughConfig(%v).TrustAllTools = %v, want %v", tt.config, got.TrustAllTools, tt.want.TrustAllTools)
			}
			if !slices.Equal(got.TrustTools, tt.want.TrustTools) {
				t.Errorf("parsePassthroughConfig(%v).TrustTools = %v, want %v", tt.config, got.TrustTools, tt.want.TrustTools)
			}
			if got.Agent != tt.want.Agent {
				t.Errorf("parsePassthroughConfig(%v).Agent = %q, want %q", tt.config, got.Agent, tt.want.Agent)
			}
		})
	}
}

// TestParsePassthroughConfig_ClonesTrustTools verifies the parsed slice does
// not alias the caller's input slice, so later mutation of the source map
// cannot corrupt adapter state.
func TestParsePassthroughConfig_ClonesTrustTools(t *testing.T) {
	t.Parallel()

	source := []any{"read", "grep"}
	pt, err := parsePassthroughConfig(map[string]any{"trust_tools": source})
	if err != nil {
		t.Fatalf("parsePassthroughConfig() error = %v", err)
	}

	source[0] = "mutated"

	if pt.TrustTools[0] != "read" {
		t.Errorf("TrustTools[0] = %q, want %q (parsed slice must not alias source)", pt.TrustTools[0], "read")
	}
}

func TestBuildArgs_FirstTurn(t *testing.T) {
	t.Parallel()

	state := &sessionState{resumeRequested: false}
	pt := passthroughConfig{
		Model:      "claude-sonnet-4.6",
		TrustTools: []string{"read", "grep"},
	}

	args := buildArgs(state, 1, "implement the fix", pt)

	wantPrefix := []string{"chat", "--no-interactive", "--wrap", "never"}
	if len(args) < len(wantPrefix) || !slices.Equal(args[:len(wantPrefix)], wantPrefix) {
		t.Errorf("buildArgs() prefix = %v, want %v", args, wantPrefix)
	}

	assertHasArgPair(t, args, "--model", "claude-sonnet-4.6")
	assertHasToken(t, args, "--trust-tools=read,grep")

	if got := args[len(args)-2]; got != "--" {
		t.Errorf("buildArgs() second-to-last token = %q, want %q", got, "--")
	}
	if got := args[len(args)-1]; got != "implement the fix" {
		t.Errorf("buildArgs() last token = %q, want the prompt %q", got, "implement the fix")
	}

	assertNoToken(t, args, "--resume")
	assertNoToken(t, args, "--require-mcp-startup")
	assertNoToken(t, args, "--mcp-config")
}

func TestBuildArgs_ExactlyOneTrustMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		pt        passthroughConfig
		wantToken string
		absent    string
	}{
		{
			name:      "trust_all_tools emits the all-tools flag only",
			pt:        passthroughConfig{TrustAllTools: true},
			wantToken: "--trust-all-tools",
			absent:    "--trust-tools=",
		},
		{
			name:      "trust_tools allowlist emits the joined token only",
			pt:        passthroughConfig{TrustTools: []string{"read", "grep"}},
			wantToken: "--trust-tools=read,grep",
			absent:    "--trust-all-tools",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args := buildArgs(&sessionState{}, 1, "p", tt.pt)
			assertHasToken(t, args, tt.wantToken)
			assertNoToken(t, args, tt.absent)
		})
	}
}

// TestBuildArgs_TrustToolsEmpty verifies that an empty trust_tools list with
// trust_all_tools false yields the single bare token "--trust-tools=", which
// trusts nothing.
func TestBuildArgs_TrustToolsEmpty(t *testing.T) {
	t.Parallel()

	args := buildArgs(&sessionState{}, 1, "p", passthroughConfig{})

	assertHasToken(t, args, "--trust-tools=")
	assertNoToken(t, args, "--trust-all-tools")
}

// TestBuildArgs_OptionalFlags verifies that model and agent flags appear only
// when configured.
func TestBuildArgs_OptionalFlags(t *testing.T) {
	t.Parallel()

	t.Run("agent flag present when configured", func(t *testing.T) {
		t.Parallel()
		args := buildArgs(&sessionState{}, 1, "p", passthroughConfig{Agent: "my-agent"})
		assertHasArgPair(t, args, "--agent", "my-agent")
	})

	t.Run("no model and no agent when unset", func(t *testing.T) {
		t.Parallel()
		args := buildArgs(&sessionState{}, 1, "p", passthroughConfig{})
		assertNoToken(t, args, "--model")
		assertNoToken(t, args, "--agent")
	})
}

// TestBuildArgs_ResumeFollowsState verifies the resume decision reads
// state.resumeRequested rather than the turn index: turn 1 with the flag set
// still emits --resume, and a later turn with the flag clear does not.
func TestBuildArgs_ResumeFollowsState(t *testing.T) {
	t.Parallel()

	t.Run("resume flag emitted once when requested", func(t *testing.T) {
		t.Parallel()
		args := buildArgs(&sessionState{resumeRequested: true}, 2, "p", passthroughConfig{})
		if n := countToken(args, "--resume"); n != 1 {
			t.Errorf("buildArgs() --resume count = %d, want 1 in [%s]", n, strings.Join(args, " "))
		}
		assertNoToken(t, args, "--resume-id")
	})

	t.Run("no resume flag when not requested", func(t *testing.T) {
		t.Parallel()
		args := buildArgs(&sessionState{resumeRequested: false}, 5, "p", passthroughConfig{})
		assertNoToken(t, args, "--resume")
	})
}

func TestKiroRegistered(t *testing.T) {
	t.Parallel()

	if !registry.Agents.Has("kiro") {
		t.Fatal(`registry.Agents.Has("kiro") = false, want true after package init`)
	}

	meta, ok := registry.Agents.Meta("kiro")
	if !ok {
		t.Fatal(`registry.Agents.Meta("kiro") reported not registered`)
	}
	if !meta.RequiresCommand {
		t.Error("AgentMeta.RequiresCommand = false, want true")
	}

	factory, err := registry.Agents.Get("kiro")
	if err != nil {
		t.Fatalf(`registry.Agents.Get("kiro") error = %v`, err)
	}
	adapter, err := factory(map[string]any{})
	if err != nil {
		t.Fatalf("factory(empty config) error = %v", err)
	}
	if _, ok := adapter.(*KiroAdapter); !ok {
		t.Errorf("factory() type = %T, want *KiroAdapter", adapter)
	}
}
