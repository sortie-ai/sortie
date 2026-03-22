package orchestrator

import (
	"errors"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// --- Test helpers ---

// stubTrackerRegistry implements the TrackerRegistry interface in
// PreflightParams with configurable Get and Meta behavior.
type stubTrackerRegistry struct {
	getFunc  func(string) (registry.TrackerConstructor, error)
	metaFunc func(string) registry.AdapterMeta
}

func (s *stubTrackerRegistry) Get(kind string) (registry.TrackerConstructor, error) {
	return s.getFunc(kind)
}

func (s *stubTrackerRegistry) Meta(kind string) registry.AdapterMeta {
	return s.metaFunc(kind)
}

// stubAgentRegistry implements the AgentRegistry interface in
// PreflightParams with configurable Get and Meta behavior.
type stubAgentRegistry struct {
	getFunc  func(string) (registry.AgentConstructor, error)
	metaFunc func(string) registry.AdapterMeta
}

func (s *stubAgentRegistry) Get(kind string) (registry.AgentConstructor, error) {
	return s.getFunc(kind)
}

func (s *stubAgentRegistry) Meta(kind string) registry.AdapterMeta {
	return s.metaFunc(kind)
}

// validPreflightParams returns a PreflightParams where all checks
// pass. Tests override individual fields to inject failures.
func validPreflightParams() PreflightParams {
	return PreflightParams{
		ReloadWorkflow: func() error { return nil },
		ConfigFunc: func() config.ServiceConfig {
			return config.ServiceConfig{
				Tracker: config.TrackerConfig{
					Kind:   "test-tracker",
					APIKey: "secret",
				},
				Agent: config.AgentConfig{
					Kind:    "test-agent",
					Command: "/usr/bin/agent",
				},
			}
		},
		TrackerRegistry: &stubTrackerRegistry{
			getFunc:  func(string) (registry.TrackerConstructor, error) { return nil, nil },
			metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
		},
		AgentRegistry: &stubAgentRegistry{
			getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
		},
	}
}

// hasCheck reports whether the result contains an error with the
// given check name.
func hasCheck(t *testing.T, result PreflightResult, check string) bool {
	t.Helper()
	for _, e := range result.Errors {
		if e.Check == check {
			return true
		}
	}
	return false
}

// requireCheck fails the test if the result does not contain an
// error with the given check name.
func requireCheck(t *testing.T, result PreflightResult, check string) {
	t.Helper()
	if !hasCheck(t, result, check) {
		t.Errorf("ValidateDispatchConfig() missing error check %q; got errors: %v", check, result.Errors)
	}
}

// requireNoCheck fails the test if the result contains an error
// with the given check name.
func requireNoCheck(t *testing.T, result PreflightResult, check string) {
	t.Helper()
	if hasCheck(t, result, check) {
		t.Errorf("ValidateDispatchConfig() has unexpected error check %q; got errors: %v", check, result.Errors)
	}
}

// --- Tests ---

func TestValidateDispatchConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		modify     func(*PreflightParams)
		wantOK     bool
		wantChecks []string // checks that MUST be present
		noChecks   []string // checks that MUST NOT be present
	}{
		{
			name:   "all valid",
			modify: func(_ *PreflightParams) {},
			wantOK: true,
		},
		{
			name: "workflow reload failure",
			modify: func(p *PreflightParams) {
				p.ReloadWorkflow = func() error { return errors.New("parse error") }
			},
			wantChecks: []string{"workflow_load"},
		},
		{
			name: "missing tracker.kind",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
			},
			wantChecks: []string{"tracker.kind"},
		},
		{
			name: "empty tracker.api_key",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind: "test-tracker",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
			},
			wantChecks: []string{"tracker.api_key"},
		},
		{
			name: "missing tracker.project when meta requires it",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresProject: true}
					},
				}
			},
			wantChecks: []string{"tracker.project"},
		},
		{
			name: "tracker.project not required when meta says so",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresProject: false}
					},
				}
			},
			wantOK:   true,
			noChecks: []string{"tracker.project"},
		},
		{
			name: "unregistered tracker kind",
			modify: func(p *PreflightParams) {
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(kind string) (registry.TrackerConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "tracker",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantChecks: []string{"tracker_adapter"},
		},
		{
			name: "missing agent.command when meta requires it",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind: "test-agent",
						},
					}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresCommand: true}
					},
				}
			},
			wantChecks: []string{"agent.command"},
		},
		{
			name: "agent.command not required when meta says so",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind: "test-agent",
						},
					}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresCommand: false}
					},
				}
			},
			wantOK:   true,
			noChecks: []string{"agent.command"},
		},
		{
			name: "missing agent.kind skips command and adapter checks",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{},
					}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(kind string) (registry.AgentConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "agent",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresCommand: true}
					},
				}
			},
			wantChecks: []string{"agent.kind"},
			noChecks:   []string{"agent.command", "agent_adapter"},
		},
		{
			name: "unregistered agent kind",
			modify: func(p *PreflightParams) {
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(kind string) (registry.AgentConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "agent",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantChecks: []string{"agent_adapter"},
		},
		{
			name: "multiple config errors collected",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(kind string) (registry.AgentConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "agent",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantChecks: []string{"tracker.kind", "tracker.api_key", "agent.kind"},
			noChecks:   []string{"workflow_load"},
		},
		{
			name: "workflow reload fails skips config checks",
			modify: func(p *PreflightParams) {
				p.ReloadWorkflow = func() error { return errors.New("file missing") }
			},
			wantChecks: []string{"workflow_load"},
			noChecks:   []string{"tracker.kind", "tracker.api_key", "tracker_adapter", "agent_adapter", "agent.kind", "agent.command", "tracker.project"},
		},
		{
			name: "zero-value meta from plain Register means no project or command error",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind: "test-agent",
						},
					}
				}
				// Both registries return zero-value meta (simulating plain Register).
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc:  func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantOK:   true,
			noChecks: []string{"tracker.project", "agent.command"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			params := validPreflightParams()
			tt.modify(&params)

			result := ValidateDispatchConfig(params)

			if tt.wantOK && !result.OK() {
				t.Fatalf("ValidateDispatchConfig() OK = false, want true; errors: %v", result.Errors)
			}
			if !tt.wantOK && len(tt.wantChecks) > 0 && result.OK() {
				t.Fatalf("ValidateDispatchConfig() OK = true, want false with checks %v", tt.wantChecks)
			}

			for _, check := range tt.wantChecks {
				requireCheck(t, result, check)
			}
			for _, check := range tt.noChecks {
				requireNoCheck(t, result, check)
			}
		})
	}
}

func TestPreflightResult_OK(t *testing.T) {
	t.Parallel()

	t.Run("zero value is OK", func(t *testing.T) {
		t.Parallel()

		var r PreflightResult
		if !r.OK() {
			t.Error("PreflightResult{}.OK() = false, want true")
		}
	})

	t.Run("with errors is not OK", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{Errors: []PreflightError{{Check: "x", Message: "bad"}}}
		if r.OK() {
			t.Error("PreflightResult{Errors: [...]}.OK() = true, want false")
		}
	})
}

func TestPreflightResult_Error(t *testing.T) {
	t.Parallel()

	t.Run("empty when OK", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{}
		if got := r.Error(); got != "" {
			t.Errorf("PreflightResult{}.Error() = %q, want %q", got, "")
		}
	})

	t.Run("single error", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{Errors: []PreflightError{
			{Check: "tracker.kind", Message: "tracker.kind is required"},
		}}
		got := r.Error()
		want := "dispatch preflight failed: tracker.kind is required"
		if got != want {
			t.Errorf("PreflightResult.Error() = %q, want %q", got, want)
		}
	})

	t.Run("multiple errors joined with semicolon", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{Errors: []PreflightError{
			{Check: "a", Message: "error one"},
			{Check: "b", Message: "error two"},
		}}
		got := r.Error()
		if !strings.HasPrefix(got, "dispatch preflight failed: ") {
			t.Errorf("PreflightResult.Error() = %q, want prefix %q", got, "dispatch preflight failed: ")
		}
		if !strings.Contains(got, "; ") {
			t.Errorf("PreflightResult.Error() = %q, want semicolon separator", got)
		}
		if !strings.Contains(got, "error one") || !strings.Contains(got, "error two") {
			t.Errorf("PreflightResult.Error() = %q, want both messages present", got)
		}
	})
}
